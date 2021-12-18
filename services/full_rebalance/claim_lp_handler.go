package full_rebalance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/go-xorm/xorm"
	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"
	"github.com/starslabhq/hermes-rebalance/alert"
	"github.com/starslabhq/hermes-rebalance/config"
	"github.com/starslabhq/hermes-rebalance/services/part_rebalance"
	"github.com/starslabhq/hermes-rebalance/tokens"
	"github.com/starslabhq/hermes-rebalance/types"
	"github.com/starslabhq/hermes-rebalance/utils"
)

const (
	vaultClaimAbi = `[{
		"inputs": [
		  {
			"internalType": "address[]",
			"name": "_strategies",
			"type": "address[]"
		  },
		  {
			"internalType": "uint256[]",
			"name": "_baseTokensAmount",
			"type": "uint256[]"
		  },
		  {
			"internalType": "uint256[]",
			"name": "_counterTokensAmount",
			"type": "uint256[]"
		  },
		  {
			"internalType": "uint256[]",
			"name": "_lpClaimIds",
			"type": "uint256[]"
		  }
		],
		"name": "claimAll",
		"outputs": [],
		"stateMutability": "nonpayable",
		"type": "function"
	  }]`
)

type lpDataGetter interface {
	getLpData(url string) (lpList *types.Data, err error)
}

type getter func(url string) (lpList *types.Data, err error)

func (g getter) getLpData(url string) (lpList *types.Data, err error) {
	return g(url)
}

type claimLPHandler struct {
	token  tokens.Tokener
	db     types.IDB
	abi    abi.ABI
	conf   *config.Config
	getter lpDataGetter
}

func newClaimLpHandler(conf *config.Config, db types.IDB, token tokens.Tokener) *claimLPHandler {
	r := strings.NewReader(vaultClaimAbi)
	abi, err := abi.JSON(r)
	if err != nil {
		logrus.Fatalf("claim abi err:%v", err)
	}
	return &claimLPHandler{
		conf:   conf,
		db:     db,
		abi:    abi,
		token:  token,
		getter: getter(getLpData),
	}
}

func (w *claimLPHandler) Name() string {
	return "full_rebalance_claim"
}

type claimParam struct {
	ChainId    int
	ChainName  string
	VaultAddr  string
	Strategies []*strategy
}

type strategy struct {
	StrategyAddr string
	BaseSymbol   string
	QuoteSymbol  string
	BaseAmount   decimal.Decimal
	QuoteAmount  decimal.Decimal
}

func strMustToDecimal(v string) decimal.Decimal {
	num, err := decimal.NewFromString(v)
	if err != nil {
		logrus.Fatalf("str to decimal err v:%s", v)
	}
	return num
}

func findParams(params []*claimParam, vaultAddr string) *claimParam {
	for _, param := range params {
		if param.VaultAddr == vaultAddr {
			return param
		}
	}
	return nil
}

func (w *claimLPHandler) getClaimParams(lps []*types.LiquidityProvider, vaults []*types.VaultInfo) (params []*claimParam, err error) {
	params = make([]*claimParam, 0)
	for _, lp := range lps {
		strategiesM := make(map[string]*strategy)
		for _, info := range lp.LpInfoList {
			if info.StrategyAddress == "" {
				return nil, fmt.Errorf("strategy empty info:%v", info)
			}
			base := strMustToDecimal(info.BaseTokenAmount)
			quote := strMustToDecimal(info.QuoteTokenAmount)

			if s, ok := strategiesM[info.StrategyAddress]; ok {
				s.BaseAmount = s.BaseAmount.Add(base)
				s.QuoteAmount = s.QuoteAmount.Add(quote)
			} else {
				s = &strategy{
					StrategyAddr: info.StrategyAddress,
					BaseSymbol:   info.BaseTokenSymbol,
					BaseAmount:   base,
					QuoteSymbol:  info.QuoteTokenSymbol,
					QuoteAmount:  quote,
				}
				strategiesM[s.StrategyAddr] = s

				addr, ok := w.getVaultAddr(s.BaseSymbol, lp.Chain, vaults)
				if !ok {
					// logrus.Errorf("vault addr not found,symbol:%s, chain:%s,vaults:%s", s.BaseSymbol, lp.Chain, b)
					return nil, fmt.Errorf("vault addr not found,symbol:%s, chain:%s", s.BaseSymbol, lp.Chain)
				}

				param := findParams(params, addr)
				if param == nil {
					param := &claimParam{
						ChainId:    lp.ChainId,
						ChainName:  lp.Chain,
						VaultAddr:  addr,
						Strategies: []*strategy{s},
					}
					params = append(params, param)
				} else {
					param.Strategies = append(param.Strategies, s)
				}
			}
		}
	}

	return params, nil
}

func powN(num decimal.Decimal, n int) decimal.Decimal {
	//10^n
	ten := decimal.NewFromFloat(10).Pow(decimal.NewFromFloat(float64(n)))
	return num.Mul(ten)
}

func decimalToBigInt(num decimal.Decimal) *big.Int {
	ret, ok := new(big.Int).SetString(num.String(), 10)
	if !ok {
		logrus.Fatalf("decimal to big.Int err num:%s", num.String())
	}
	return ret
}

func (w *claimLPHandler) createTxTask(tid uint64, params []*claimParam) ([]*types.TransactionTask, error) {
	var tasks []*types.TransactionTask
	for _, param := range params {
		var (
			addrs    []common.Address
			bases    []*big.Int
			quotes   []*big.Int
			claimIds []*big.Int
		)

		for _, s := range param.Strategies {
			addr := common.HexToAddress(s.StrategyAddr)
			addrs = append(addrs, addr)

			//base
			decimal0, ok := w.token.GetDecimals(param.ChainName, s.BaseSymbol)
			if !ok {
				logrus.Fatalf("unexpectd decimal bseSymbol:%s", s.BaseSymbol)
			}
			baseDecimal := powN(s.BaseAmount, decimal0)
			base := decimalToBigInt(baseDecimal)
			bases = append(bases, base)

			//quote
			decimal1, ok := w.token.GetDecimals(param.ChainName, s.QuoteSymbol)
			if !ok {
				logrus.Fatalf("unexpectd decimal quoteSymbol:%s,chain:%s", s.QuoteSymbol, param.ChainName)
			}
			quoteDecimal := powN(s.QuoteAmount, decimal1)
			quote := decimalToBigInt(quoteDecimal)
			quotes = append(quotes, quote)
			claimIds = append(claimIds, big.NewInt(0))
		}
		logrus.Infof("claimAll tid:%d,addrs:%v,bases:%v,quotes:%v,claimIds:%v", tid, addrs, bases, quotes, claimIds)
		input, err := w.abi.Pack("claimAll", addrs, bases, quotes, claimIds)
		if err != nil {
			return nil, fmt.Errorf("claim pack err:%v", err)
		}
		encoded, _ := json.Marshal(param)
		chain, ok := w.conf.Chains[strings.ToLower(param.ChainName)]
		if !ok {
			logrus.Fatalf("get from addr empty chainName:%s", param.ChainName)
		}
		task := &types.TransactionTask{
			FullRebalanceId: tid,
			BaseTask:        &types.BaseTask{State: int(types.TxUnInitState)},
			TransactionType: int(types.ClaimFromVault),
			ChainId:         param.ChainId,
			ChainName:       param.ChainName,
			From:            chain.BridgeAddress,
			To:              param.VaultAddr,
			Params:          string(encoded),
			InputData:       hexutil.Encode(input),
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (w *claimLPHandler) updateState(fullTask *types.FullReBalanceTask, state types.FullReBalanceState) error {
	fullTask.State = state
	return w.db.UpdateFullReBalanceTask(w.db.GetEngine(), fullTask)
}

func (w *claimLPHandler) insertTxTasksAndUpdateState(txTasks []*types.TransactionTask,
	fullTask *types.FullReBalanceTask, state types.FullReBalanceState) error {
	err1 := utils.CommitWithSession(w.db, func(s *xorm.Session) error {
		err := w.db.SaveTxTasks(s, txTasks)
		if err != nil {
			return fmt.Errorf("claim save txtasks err:%v,tid:%d", err, fullTask.ID)
		}
		fullTask.State = state
		err = w.db.UpdateFullReBalanceTask(s, fullTask)
		if err != nil {
			return fmt.Errorf("update claim task err:%v,tid:%d", err, fullTask.ID)
		}
		return nil
	})
	return err1
}

func (w *claimLPHandler) getVaultAddr(tokenSymbol, chain string, vaults []*types.VaultInfo) (string, bool) {
	currency := w.token.GetCurrency(chain, tokenSymbol)
	for _, vault := range vaults {
		if vault.Currency == currency {
			c, ok := vault.ActiveAmount[chain]
			if !ok {
				b, _ := json.Marshal(vault)
				logrus.Fatalf("vault activeAmount not found chain:%s,vault:%s", chain, b)
			}
			return c.ControllerAddress, true
		}
	}
	return "", false
}

func (w *claimLPHandler) Do(task *types.FullReBalanceTask) error {

	data, err := w.getter.getLpData(w.conf.ApiConf.LpUrl)
	if err != nil {
		return fmt.Errorf("claim get lpData err:%v", err)
	}

	var lps = data.LiquidityProviderList
	if len(lps) == 0 {
		return nil
	}
	if len(data.VaultInfoList) == 0 {
		return fmt.Errorf("lp data valutlist empty")
	}
	params, err := w.getClaimParams(lps, data.VaultInfoList)
	if err != nil {
		b0, _ := json.Marshal(lps)
		b1, _ := json.Marshal(data.VaultInfoList)
		logrus.Errorf("get claim params err:%v,lps:%s,vaults:%s,tid:%d", err, b0, b1, task.ID)
		return err
	}
	//TODO 考虑空数组的情况
	txTasks, err := w.createTxTask(task.ID, params)
	if err != nil {
		b, _ := json.Marshal(params)
		logrus.Errorf("create tx task err:%v,params:%s,tid:%d", err, b, task.ID)
		return err
	}
	if len(txTasks) == 0 {
		err = w.updateState(task, types.FullReBalanceClaimLP)
		if err != nil {
			return fmt.Errorf("update claim state err:%v,tid:%d", err, task.ID)
		}
	}
	txTasks, err = part_rebalance.SetNonceAndGasPrice(txTasks)
	if err != nil {
		logrus.Errorf("set gas_price and fee err:%v,tid:%d", err, task.ID)
		return err
	}
	err = w.insertTxTasksAndUpdateState(txTasks, task, types.FullReBalanceClaimLP)
	if err == nil {
		w.stateChanged(types.FullReBalanceClaimLP, txTasks, task)
	}
	return err
}

func (w *claimLPHandler) getTxTasks(fullRebalanceId uint64) ([]*types.TransactionTask, error) {
	tasks, err := w.db.GetTransactionTasksWithFullRebalanceId(fullRebalanceId, types.ClaimFromVault)
	return tasks, err
}

func (w *claimLPHandler) CheckFinished(task *types.FullReBalanceTask) (finished bool, nextState types.FullReBalanceState, err error) {
	txTasks, err := w.getTxTasks(task.ID)
	if err != nil {
		return false, types.FullReBalanceClaimLP, fmt.Errorf("full_r get txTasks err:%v", err)
	}
	taskCnt := len(txTasks)
	//TODO 假设没有需要claim的 这里应该就是0
	if taskCnt == 0 {
		logrus.Infof("claim txTasks size  zero tid:%d", task.ID)
		return true, types.FullReBalanceMarginBalanceTransferOut, nil
	}
	var (
		sucCnt  int
		failCnt int
	)
	failed := make([]*types.TransactionTask, 0)
	for _, task := range txTasks {
		if task.State == int(types.TxSuccessState) {
			sucCnt++
		}
		if task.State == int(types.TxFailedState) {
			logrus.Warnf("call claim fail tx_task_id:%d", task.ID)
			failed = append(failed, task)
			failCnt++
		}
	}
	if sucCnt == taskCnt {
		w.stateChanged(types.FullReBalanceMarginBalanceTransferOut, txTasks, task)
		return true, types.FullReBalanceMarginBalanceTransferOut, nil
	}
	if failCnt != 0 {
		logrus.Warnf("claim lp handler failed tid:%d", task.ID)

		w.stateChanged(types.FullReBalanceFailed, failed, task)

		return false, types.FullReBalanceFailed, nil
	}
	return false, types.FullReBalanceClaimLP, nil
}

const claimTemp = `
stage: {{.Stage}}
full_id: {{.FullID}}
full_params: {{.FullParam}}
{{with .Txs }}
{{ range . }}
type: {{.TransactionType}}
nonce: {{.Nonce}}
gas_price: {{.GasPrice}}
gas_limit: {{.GasLimit}}
amount: {{.Amount}}
chain_id: {{.ChainId}}
chain_name: {{.ChainName}}
from: {{.From}}
to: {{.To}}
---------------------
{{- end }}
{{end}}
`

type ClaimMsg struct {
	Stage     string
	FullParam string
	FullID    uint64
	Txs       []*types.TransactionTask
}

func createClaimMsg(stage string, txTasks []*types.TransactionTask, fullTask *types.FullReBalanceTask) (string, error) {
	msg := &ClaimMsg{
		FullParam: fullTask.Params,
		FullID:    fullTask.ID,
		Stage:     stage,
		Txs:       txTasks,
	}
	t := template.New("claimlp")
	temp := template.Must(t.Parse(claimTemp))
	buf := &bytes.Buffer{}
	err := temp.Execute(buf, msg)
	if err != nil {
		return "", fmt.Errorf("excute temp err:%v", err)
	}
	return buf.String(), nil
}

func (w *claimLPHandler) stateChanged(next types.FullReBalanceState, txTasks []*types.TransactionTask, fullTask *types.FullReBalanceTask) {
	var (
		msg   string
		stage string
		err   error
	)
	switch next {
	case types.FullReBalanceClaimLP:
		stage = "claimlp_tx_created"
		msg, err = createClaimMsg(stage, txTasks, fullTask)
		if err != nil {
			logrus.Errorf("create claim msg err:%v,stage:%s", err, stage)
		}
		err = alert.Dingding.SendMessage("claimlp", msg)
		if err != nil {
			logrus.Errorf("send claimlp tx_created err:%v", err)
		}
	case types.FullReBalanceFailed:
		stage = "claimlp_failed"
		msg, err = createClaimMsg(stage, txTasks, fullTask)
		if err != nil {
			logrus.Errorf("create claim msg err:%v,stage:%s", err, stage)
		}
		err = alert.Dingding.SendAlert("claimlp", msg, []string{})
		if err != nil {
			logrus.Errorf("send calimlp failed err:%v", err)
		}
	case types.FullReBalanceMarginBalanceTransferOut:
		stage = "claimlp_suc"
		msg, err = createClaimMsg(stage, txTasks, fullTask)
		if err != nil {
			logrus.Errorf("create claim msg err:%v,stage:%s", err, stage)
		}
		err = alert.Dingding.SendMessage("claimlp", msg)
		if err != nil {
			logrus.Errorf("send calimlp failed err:%v", err)
		}
	}

}

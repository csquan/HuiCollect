package types

import (
	"encoding/json"
	"github.com/sirupsen/logrus"
	"time"
)

type Base struct {
	ID        uint64    `json:"id" gorm:"primary_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type BaseTask struct {
	State   int
	Message string
}

type PartReBalanceState = int

const (
	PartReBalanceInit PartReBalanceState = iota
	PartReBalanceCross
	PartReBalanceTransferIn
	PartReBalanceInvest
	PartReBalanceSuccess
	PartReBalanceFailed
)

type PartReBalanceTask struct {
	*Base
	*BaseTask
	Params string `xorm:"params"`
}

func (p *PartReBalanceTask) ReadParams() (params *Params, err error) {
	params = &Params{}
	if err = json.Unmarshal([]byte(p.Params), params); err != nil {
		logrus.Errorf("Unmarshal PartReBalanceTask params error:%v task:[%v]", err, p)
		return
	}

	return
}

type AssetTransferTask struct {
	*Base
	*BaseTask
	RebalanceId  uint64 `xorm:"rebalance_id"`
	TransferType uint8  `xorm:"transfer_type"`
	Progress     string `xorm:"progress"`
	Params       string `xorm:"params"`
}

type TransactionTask struct {
	*Base
	*BaseTask
	RebalanceId     uint64 `xorm:"rebalance_id"`
	TransferId      uint   `xorm:"transfer_id"`
	Nonce           int    `xorm:"nonce"`
	ChainId         int    `xorm:"chain_id"`
	Decimal         int    `xorm:"decimal"`
	From            string `xorm:"from"`
	To              string `xorm:"to"`
	State           int    `xorm:"state"`
	ContractAddress string `xorm:"contract_address"`
	Value           int    `xorm:"value"`
	Input_data      string `xorm:"input_data"`
	Cipher          string `xorm:"cipher"`
	EncryptData     string `xorm:"encryptData"`
	SignData        []byte `xorm:"signed_data"`
	Params          string `xorm:"params"`
	Hash            string `xorm:"hash"`
}

type InvestTask struct {
	*Base
	*BaseTask
	RebalanceId uint64 `xorm:"rebalance_id"`
}

type CrossTask struct {
	*Base
	*BaseTask
	RebalanceId   uint64 `xorm:"rebalance_id"`
	ChainFrom     string `xorm:"chain_from"`
	ChainFromAddr string `xorm:"chain_from_addr"`
	ChainTo       string `xorm:"chain_to"`
	ChainToAddr   string `xorm:"chain_to_addr"`
	CurrencyFrom  string `xorm:"currency_from"`
	CurrencyTo    string `xorm:"currency_to"`
	Amount        string `xorm:"amount"`
	TaskNo        uint64
}

type CrossSubTask struct {
	*Base
	*BaseTask
	TaskNo       uint64 `xorm:"task_no"`
	BridgeTaskId uint64 `xorm:"bridge_task_id"` //跨链桥task_id
	ParentTaskId uint64 `xorm:"cross_task_id"`  //父任务id
	ChainFrom    string `xorm:"chain_from"`
	ChainTo      string `xorm:"chain_to"`
	CurrencyFrom string `xorm:"currency_from"`
	CurrencyTo   string `xorm:"currency_to"`
	Amount       string `xorm:"amount"`
}

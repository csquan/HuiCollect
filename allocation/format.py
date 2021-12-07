# -*- coding:utf-8 -*-
import traceback
import requests
import yaml
import json
import utils
import time

from functools import reduce
from decimal import *
from sqlalchemy.orm import sessionmaker
from orm import *

targetChain = ['bsc', 'polygon']


def get_pool_info(url):
    # 存储从api获取的poolinfo
    ret = requests.get(url)
    string = str(ret.content, 'utf-8')
    print(string)
    e = json.loads(string)

    return e['data']


def read_yaml(path):
    with open(path, 'r', encoding='utf8') as f:
        return yaml.safe_load(f.read())


def format_addr(addr):
    if addr.startswith('0x'):
        return addr.lower()
    else:
        return ('0x' + addr).lower()


def format_token_name(currency_name_set, name):
    for k in currency_name_set:
        if name.lower().find(k) >= 0:
            return True, k

    return False, name


class Project:
    def __init__(self, chain, name, url):
        self.chain = chain
        self.name = name
        self.url = url

    def get_info(self):
        res = requests.get(self.url)
        if res.status_code != 200:
            print("re url服务异常")
            sys.exit(1)
        string = str(res.content, 'utf-8')
        print(string)
        e = json.loads(string)
        return e['data']


def generate_strategy_key(chain, project, currencies):
    currencies = list(filter(lambda x: x is not None, currencies))
    currencies.sort(key=len, reverse=True)
    return "{}_{}_{}".format(chain, project, '_'.join(currencies))


def calc(conf, session, currencies):
    res = {}

    # 注意usdt与usd的区分，别弄混了
    currency_names = [k for k in sorted(currencies.keys(), key=len, reverse=True)]

    print("currencies:{}".format(currencies))

    # 获取rebalance所需业务信息
    re_balance_input_info = get_pool_info(conf['pool']['url'])

    threshold_org = re_balance_input_info['threshold']
    vault_info_list = re_balance_input_info['vaultInfoList']

    # 整理出阈值，当前值 进行比较 {usdt:{bsc:{amount:"1", controllerAddress:""}}}
    threshold_format = {}
    account_info = {}
    strategy_addresses = {}

    # 计算跨链的初始状态
    for vault in vault_info_list:
        (ok, name) = format_token_name(currency_names, vault['tokenSymbol'])
        if ok:
            for chain in vault['activeAmount']:
                if name not in account_info:
                    account_info[name] = {}

                account_info[name][chain.lower()] = vault['activeAmount'][chain]
                account_info[name][chain.lower()]['amount'] = Decimal(account_info[name][chain.lower()]['amount'])
                account_info[name][chain.lower()]['controller'] = account_info[name][chain.lower()]['controllerAddress']

            for chain, proj_dict in vault['strategies'].items():
                for project, st_list in proj_dict.items():
                    for st in st_list:
                        tokens = [format_token_name(currency_names, c)[1] for c in st['tokenSymbol'].split('-')]
                        strategy_addresses[generate_strategy_key(chain.lower(), project.lower(), tokens)] = st[
                            'strategyAddress']

    account_info['usdt'] = {
        'bsc': {
            'amount': Decimal(0),
            'controller': ''
        },
        'heco': {
            'amount': Decimal(10000000),
            'controller': ''
        },
        'polygon': {
            'amount': Decimal(0),
            'controller': ''
        }
    }

    account_info['eth'] = {
        'bsc': {
            'amount': Decimal(100),
            "controller": ""
        },
        'heco': {
            'amount': Decimal(0),
            "controller": ""
        },
        'polygon': {
            'amount': Decimal(0),
            "controller": ""
        }
    }
    account_info['btc'] = {
        'bsc': {
            'amount': Decimal(10),
            "controller": ""
        },
        'heco': {
            'amount': Decimal(0),
            "controller": ""
        },
        'polygon': {
            'amount': Decimal(0),
            "controller": ""
        }
    }
    #
    # account_info['cake'] = {
    #     'bsc': {
    #         'amount': Decimal(0),
    #     }
    # }
    #
    # account_info['bnb'] = {
    #     'bsc': {
    #         'amount': Decimal(2000),
    #     }
    # }
    print("init balance info for cross:{}".format(account_info))
    print("strategy address info:{}".format(strategy_addresses))

    # 计算阈值
    for threshold in threshold_org:
        (ok, name) = format_token_name(currency_names, threshold['tokenSymbol'])
        if ok:
            threshold_format[name] = threshold['thresholdAmount']
    print("threshold info after format:{}".format(threshold_format))

    # 比较阈值
    need_re_balance = False
    for name in threshold_format:
        # 没有发现相关资产
        if name not in account_info:
            continue

        total = Decimal(0)
        for item in account_info[name].values():
            total += item['amount']

        need_re_balance = total > Decimal(threshold_format[name])
        if need_re_balance:
            break

    # 没超过阈值
    if not need_re_balance:
        return

    # 获取apr等信息
    projects = [Project(p['chain'], p['name'], p['url']) for p in conf['project']]

    # chain_project_coin1_coin2
    apr = {}
    price = {}
    daily_reward = {}
    tvl = {}

    for p in projects:
        info = p.get_info()

        for pool in info:
            for token in pool['rewardTokenList'] + pool['depositTokenList']:
                currency = find_currency_by_address(session, format_addr(token['tokenAddress']))
                if currency is not None and currency not in price:
                    price[currency] = Decimal(token['tokenPrice'])

                names = [format_token_name(currency_names, c)[1] for c in pool['poolName'].split("/")]
                key = generate_strategy_key(p.chain, p.name, names)

                apr[key] = Decimal(pool['apr'])
                tvl[key] = Decimal(pool['tvl'])
                daily_reward[key] = reduce(lambda x, y: x + y,
                                           map(lambda t: Decimal(t['tokenPrice']) * Decimal(t['dayAmount']),
                                               pool['rewardTokenList']))

    print("apr info:{}".format(apr))
    print("price info:{}".format(price))
    print("daily reward info:{}".format(daily_reward))
    print("tvl info:{}".format(tvl))

    # 计算跨链的最终状态
    after_balance_info = {}
    for currency in account_info:
        strategies = {}
        caps = {}
        for chain in ['bsc', 'polygon']:
            strategies[chain] = find_strategies_by_chain_and_currency(session, chain, currency)
            caps[chain] = Decimal(0)

            for s in strategies[chain]:
                # 先忽略单币
                if s.currency1 is None:
                    continue

                key = generate_strategy_key(s.chain, s.project, [s.currency0, s.currency1])
                if key not in apr or apr[key] < Decimal(0.18):
                    continue

                caps[chain] += (daily_reward[key] * Decimal(365) / Decimal(0.18) - tvl[key])

        total = Decimal(0)
        for item in account_info[currency].values():
            total += item['amount']

        caps_total = sum(caps.values())
        for k, v in caps.items():
            if v > 0:
                after_balance_info[currency] = {
                    k: str(total * v / caps_total)
                }

    print("calc final state:{}", after_balance_info)

    # 跨链信息
    balance_diff_map = {}
    # cross list
    res['cross_balances'] = []
    # send to bridge
    res['send_to_bridge_params'] = []
    res['receive_from_bridge_params'] = []

    # 生成跨链参数, 需要考虑最小值
    for currency in after_balance_info:
        for chain in ['bsc', 'polygon']:
            if chain not in after_balance_info[currency] or currencies[currency].min is None:
                continue

            diff = Decimal(after_balance_info[currency][chain]) - account_info[currency][chain]['amount']
            if diff > Decimal(currencies[currency].min) or diff < Decimal(
                    currencies[currency].min) * -1:
                if currency not in balance_diff_map:
                    balance_diff_map[currency] = {}
                balance_diff_map[currency][chain] = diff.quantize(
                    Decimal(10) ** (-1 * currencies[currency].crossDecimal),
                    ROUND_DOWN)  # format to min decimal

    print("diff map:{}", balance_diff_map)

    def add_cross_item(currency, from_chain, to_chain, amount):
        if amount > Decimal(currencies[currency].min):
            token_decimal = currencies[currency].tokens[from_chain].decimal

            account_info[currency][from_chain]['amount'] -= amount
            account_info[currency][to_chain]['amount'] += amount

            res['cross_balances'].append({
                'from_chain': from_chain,
                'to_chain': to_chain,
                'from_addr': conf['bridge_port'][from_chain],
                'to_addr': conf['bridge_port'][to_chain],
                'from_currency': currencies[currency].tokens[from_chain].crossSymbol,
                'to_currency': currencies[currency].tokens[to_chain].crossSymbol,
                'amount': amount,
            })

            task_id = '{}'.format(time.time_ns() * 100)
            res['send_to_bridge_params'].append({
                'chain_name': from_chain,
                'chain_id': conf['chain'][from_chain],
                'from': conf['bridge_port'][from_chain],
                'to': account_info[currency][from_chain]['controller'],
                'bridge_address': conf['bridge_port'][from_chain],
                'amount': amount * (Decimal(10) ** token_decimal),
                'task_id': task_id
            })
            res['receive_from_bridge_params'].append({
                'chain_name': to_chain,
                'chain_id': conf['chain'][to_chain],
                'from': conf['bridge_port'][to_chain],
                'to': account_info[currency][to_chain]['controller'],
                "erc20_contract_addr": currencies[currency].tokens[from_chain].address,
                'amount': amount * (Decimal(10) ** token_decimal),
                'task_id': task_id,
            })

    for currency in balance_diff_map:
        target_chain = {
            'bsc': 'polygon',
            'polygon': 'bsc',
        }

        for chain in balance_diff_map[currency]:
            diff = balance_diff_map[currency][chain]

            if diff < 0:
                add_cross_item(currency, chain, target_chain[chain], diff * -1)

            else:
                if account_info[currency]['heco']['amount'] > diff:
                    add_cross_item(currency, 'heco', chain, diff)
                else:

                    add_cross_item(currency, target_chain[chain], chain,
                                   (diff - account_info[currency]['heco']['amount']).quantize(
                                       Decimal(10) ** (-1 * currencies[currency].crossDecimal),
                                       ROUND_DOWN))

                    add_cross_item(currency, 'heco', chain, account_info[currency]['heco']['amount'].quantize(
                        Decimal(10) ** (-1 * currencies[currency].crossDecimal),
                        ROUND_DOWN))

    # receive from bridge

    print("cross info:{}", json.dumps(res, cls=utils.DecimalEncoder))

    res['invest_params'] = []

    strategyAddresses = []
    baseTokenAmount = []
    counterTokenAmount = []

    for chain in targetChain:
        info = calc_invest(session, chain, account_info, price, daily_reward, apr, tvl)
        for strategy, amounts in info.items():
            info1 = get_info_by_strategy_str(strategy)
            # 从strategy_addresses里面根据key：strategy查找
            strategyAddresses.append(strategy_addresses[strategy])
            # amounts 有两个币种对应的值，需要区分base和counter
            baseTokenAmount.append(0)
            counterTokenAmount.append(0)

    """
    res['invest_params'].append({
        'chain_name': to_chain,
        'chain_id': conf['chain'][to_chain],
        'from': conf['bridge_port'][to_chain],
        'to': account_info[currency][to_chain]['controller'],

        "strategyAddresses": currencies[currency].tokens[from_chain].address,
        'baseTokenAmount': amount * (Decimal(10) ** token_decimal),
        'counterTokenAmount': task_id,
    })
    """

    return res


def get_info_by_strategy_str(lp):
    data = lp.split('_')
    if len(data) < 3:
        return None, None
    elif len(data) == 3:
        return data, data[2], None
    else:
        return data, data[2], data[3]


def calc_invest(session, chain, balance_info_dict, price_dict, daily_reward_dict, apr_dict, tvl_dict):
    # 项目列表

    def f(key):
        infos = get_info_by_strategy_str(key)
        strategies = [s for s in
                      find_strategies_by_chain_project_and_currencies(session, chain, infos[0][1], infos[1], infos[2])]
        return len(strategies) > 0

    invest_calc_result = {}

    detla = Decimal(0.005)

    while True:
        lpKeys = [k for k in sorted(apr_dict, key=apr_dict.get, reverse=True)]
        lpKeys = list(filter(f, lpKeys))
        if len(lpKeys) <= 0:
            break

        # 找到top1 与top2
        top = []
        apr1 = apr_dict[lpKeys[0]]
        aprTarget = Decimal(0.01)
        for key in lpKeys:

            if abs(apr_dict[key] - apr1) < detla:
                top.append(key)
            else:
                aprTarget = apr_dict[key]
                break

        for key in top:
            filled, vol, changes = fill_cap(chain, key, daily_reward_dict, tvl_dict, balance_info_dict, price_dict,
                                            max(aprTarget, apr1 - detla))

            if key not in invest_calc_result:
                invest_calc_result[key] = {}

            for k, v in changes.items():
                if k not in invest_calc_result[key]:
                    invest_calc_result[key][k] = Decimal(0)

                invest_calc_result[key][k] += v * -1
                balance_info_dict[k][chain]['amount'] += v

            tvl_dict[key] += vol
            apr_dict[key] = daily_reward_dict[key] * 365 / tvl_dict[key]

            # 说明没有对应资产了
            if not filled:
                lpKeys.remove(key)
                apr_dict.pop(key)

    for k in [key for key in invest_calc_result.keys()]:
        if len(invest_calc_result[k].keys()) == 0:
            invest_calc_result.pop(k)

    print('invest info:{}'.format(json.dumps(invest_calc_result, cls=utils.DecimalEncoder)))

    return invest_calc_result


def get_balance(balance_dict, currency, chain):
    if currency not in balance_dict:
        return Decimal(0)
    if chain not in balance_dict[currency]:
        return Decimal(0)
    return balance_dict[currency][chain]['amount']


def get_price(price_dict, currency):
    if currency not in price_dict:
        return Decimal(0)

    return price_dict[currency]


# 返回值 1. 是否填满了，2 填充量是多少 3.资产余额变化
def fill_cap(chain, strategy, daily_reward_dict, tvl_dict, balance_dict, price_dict, target_apr):
    cap = (daily_reward_dict[strategy] * Decimal(365) / target_apr - tvl_dict[strategy])
    data, c0, c1 = get_info_by_strategy_str(strategy)
    if c0 is None:
        return False, 0, {}

    # 单币
    if c1 is None:
        if get_price(price_dict, c0) == Decimal(0):
            return False, 0, {}

        vol = min(cap / get_price(price_dict, c0), get_balance(balance_dict, c0, chain))
        if vol <= 0:
            return False, 0, {}

        return cap == vol, vol, {c0: -1 * vol}

    # 双币
    vol = min(get_balance(balance_dict, c0, chain) * get_price(price_dict, c0),
              get_balance(balance_dict, c1, chain) * get_price(price_dict, c1), cap / 2)

    if vol <= 0:
        return False, 0, {}

    amount0 = vol / get_price(price_dict, c0)
    amount1 = vol / get_price(price_dict, c1)
    if amount0 > 0 and amount1 > 0:
        return cap == vol * 2, vol * 2, {c0: - amount0, c1: -amount1}

    return False, 0, {}


if __name__ == '__main__':
    # 读取config
    conf = read_yaml("./wang.yaml")

    db = create_engine(conf['db'],
                       encoding='utf-8',  # 编码格式
                       echo=False,  # 是否开启sql执行语句的日志输出
                       pool_recycle=-1  # 多久之后对线程池中的线程进行一次连接的回收（重置） （默认为-1）,其实session并不会被close
                       )
    session = sessionmaker(db)()

    currencies = {x.name: x for x in session.query(Currency).all()}

    for t in session.query(Token).all():
        curr = currencies[t.currency]
        if not hasattr(curr, 'tokens'):
            curr.tokens = {}
        curr.tokens[t.chain] = t

    session.close()

    while True:
        time.sleep(3)
        try:
            session = sessionmaker(db)()

            # 已经有小re了
            # tasks = find_part_re_balance_open_tasks(session)
            # if tasks is not None:
            #    continue

            params = calc(conf, session, currencies)
            if params is None:
                continue

            create_part_re_balance_task(session, json.dumps(params, cls=utils.DecimalEncoder))
            session.commit()

        except Exception as e:
            print("except happens:{}".format(e))
            # print("{}".format(e.__traceback__))
            print(traceback.format_exc())
        finally:
            session.close()

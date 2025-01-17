package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"main/base"
	"main/node_manager"
	"math/big"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/polynetwork/bridge-common/chains/eth"
	"github.com/polynetwork/bridge-common/log"
)

type Context struct {
	nodes *eth.SDK
	sync.RWMutex
	height        uint64
	getRewardsUrl string
	getGasFeeUrl  string
}

func (ctx *Context) Till(height uint64) {
	pass := false
	for {
		ctx.RLock()
		pass = ctx.height >= height
		ctx.RUnlock()
		if pass {
			return
		}
		time.Sleep(time.Millisecond * 300)
	}
}

type Case struct {
	index   int64
	err     error
	actions []Action
	plan    []*ActionItem
}

func (c *Case) Run(ctx *Context) (err error) {
	exit := make(chan struct{})
	defer func() {
		close(exit)
	}()

	go func() {
		for {
			select {
			case <-exit:
				return
			default:
				height, err := ctx.nodes.Node().GetLatestHeight()
				if err != nil {
					log.Error("Get chain latest height failed", "err", err)
				} else {
					ctx.Lock()
					ctx.height = height
					ctx.Unlock()
				}
				time.Sleep(time.Millisecond * 200)
			}
		}
	}()

	// Sort actions
	bp := make(map[uint64][]Action)
	for i, a := range c.actions {
		a.SetIndex(i)
		bp[a.StartAt()] = append(bp[a.StartAt()], a)
	}
	c.plan = make([]*ActionItem, 0, len(bp))
	for b, actions := range bp {
		c.plan = append(c.plan, &ActionItem{b, actions})
	}
	sort.Slice(c.plan, func(i, j int) bool { return c.plan[i].start < c.plan[j].start })

	type result struct {
		note  string
		err   error
		index int
	}
	res := make(chan result, len(c.actions))
	for i, item := range c.plan {
		log.Info("Scheduling plan", "index", i, "action_count", len(item.actions), "at", item.start)
		for _, action := range item.actions {
			go func(a Action) {
				ctx.Till(a.StartAt())
				log.Info("Running case action", "case_index", c.index, "action_index", a.Index())
				note, e := a.Run(ctx)
				res <- result{note, e, a.Index()}
			}(action)
		}
	}

	for j := 0; j < len(c.actions); j++ {
		log.Info("Waiting case actions result", "case_index", c.index, "progress", j+1, "total", len(c.actions))
		r := <-res
		c.actions[r.index].SetError(r.err)
		c.actions[r.index].SetNote(r.note)
		if r.err != nil {
			err = fmt.Errorf("action failure, err: %v, action_index: %v", r.err, r.index)
			log.Error("Run case action failed", "case", c.index, "action", r.index, "err", err)
		}
	}
	return
}

type ActionItem struct {
	start   uint64
	actions []Action
}

type Action interface {
	Run(*Context) (string, error)
	StartAt() uint64
	Before() uint64
	SetIndex(int)
	Index() int
	Error() error
	SetError(err error)
	Note() string
	SetNote(note string)
}

type ActionBase struct {
	Epoch        uint64
	Block        uint64
	ShouldBefore uint64
	index        int
	note         string
	err          error
}

func (a *ActionBase) StartAt() uint64 { return a.Block + a.Epoch*uint64(CONFIG.BlocksPerEpoch) }
func (a *ActionBase) Before() uint64 {
	return a.ShouldBefore + a.Epoch*uint64(CONFIG.BlocksPerEpoch)
}
func (a *ActionBase) SetIndex(index int)  { a.index = index }
func (a *ActionBase) Index() int          { return a.index }
func (a *ActionBase) SetError(err error)  { a.err = err }
func (a *ActionBase) Error() error        { return a.err }
func (a *ActionBase) SetNote(note string) { a.note = note }
func (a *ActionBase) Note() string        { return a.note }

type SendTx struct {
	ActionBase
	Tx            *types.Transaction
	ShouldSucceed bool
}

func (a *SendTx) Run(ctx *Context) (note string, err error) {
	node := ctx.nodes.Node()
	err = node.SendTransaction(context.Background(), a.Tx)
	if err != nil {
		fmt.Printf("SendTransaction err: %s\n", err)
		return
	}
	currentHeight, _ := node.GetLatestHeight()
	log.Info("Sent tx", "current_height", currentHeight, "hash", a.Tx.Hash().Hex(), "index", a.Index(), "node", node.Address())
	for i := 0; i < 10; i++ {
		time.Sleep(time.Second * 2)
		height, _, pending, err := node.Confirm(a.Tx.Hash(), 1, 10)
		if err != nil {
			return "", err
		}

		if height > 0 {
			if height <= a.Before() {
				rec, err := node.TransactionReceipt(context.Background(), a.Tx.Hash())
				if err != nil {
					return "", err
				}
				if (rec.Status == 1) == a.ShouldSucceed {
					return "", nil
				}
				return "", fmt.Errorf("transaction status error, status %v, wanted %v", rec.Status == 1, a.ShouldSucceed)
			}
			return "", fmt.Errorf("tx packed too late, height %v, expected before %v", height, a.Before())
		} else if !pending {
			return "", fmt.Errorf("possible tx lost %s", a.Tx.Hash())
		}
	}
	return "", nil
}

type Query struct {
	ActionBase
	Request    ethereum.CallMsg
	Assertions []Assertion
}

func (a *Query) Run(ctx *Context) (note string, err error) {
	node := ctx.nodes.Node()
	fmt.Printf("action inedx:%d, node:%s\n", a.index, node.Address())
	output, err := node.CallContract(context.Background(), a.Request, big.NewInt(int64(a.StartAt())))
	if err != nil {
		fmt.Printf("CallContract err: %s\n", err)
		return
	}
	err = Assert(output, a.Assertions)
	return
}

type CheckBalance struct {
	ActionBase
	Address             common.Address
	Validators          []common.Address
	NetStake            *big.Int
	CheckCommission     bool
	CommissionValidator common.Address
}

func (a *CheckBalance) Run(ctx *Context) (note string, err error) {
	checkHeight := a.StartAt() - 10
	fmt.Println("action ", a.Index(), "check balance at height ", checkHeight)
	balance, err := ctx.nodes.Node().BalanceAt(context.Background(), a.Address, big.NewInt(int64(checkHeight)))
	if err != nil {
		return "", err
	}
	fmt.Printf("account=%s balance=%s\n", a.Address.String(), balance)

	initialBalance := new(big.Int)
	if b, ok := base.InitialBalanceMap[a.Address.String()]; ok {
		initialBalance.SetString(b, 10)
	}
	fmt.Printf("account=%s initialBalance=%s\n", a.Address.String(), initialBalance)

	fmt.Printf("account=%s netStake=%s\n", a.Address.String(), a.NetStake)

	expectedRewards, err := a.getExpectedRewards(ctx, a.Address, checkHeight)
	if err != nil {
		return
	}
	fmt.Printf("account=%s expectedRewards=%s\n", a.Address.String(), expectedRewards)
	gasFee, err := a.getGasFee(ctx, a.Address, checkHeight)
	if err != nil {
		return
	}
	fmt.Printf("account=%s gasFee=%s\n", a.Address.String(), gasFee)

	// get unArrivedRewards
	unArrivedRewards := big.NewInt(0)
	for _, validator := range a.Validators {
		input := &node_manager.GetStakeRewardsParam{ConsensusAddress: validator, StakeAddress: a.Address}
		data, err := input.Encode()
		if err != nil {
			err = fmt.Errorf("encode failed, err: %v", err)
			return "", err
		}
		request := ethereum.CallMsg{To: &NODE_MANAGER_CONTRACT, Data: data}
		output, err := ctx.nodes.Node().CallContract(context.Background(), request, big.NewInt(int64(checkHeight)))
		if err != nil {
			log.Error("callContract failed", "err", err)
			continue
		}
		unpacked, err := node_manager.ABI.Unpack(base.MethodGetStakeRewards, output)
		if err != nil {
			return "", fmt.Errorf("fail to unpack output: %v %x", err, output)
		}

		result := *abi.ConvertType(unpacked[0], new([]byte)).(*[]byte)
		stakeWards := &node_manager.StakeRewards{}
		err = rlp.DecodeBytes(result, stakeWards)
		if err != nil {
			return "", fmt.Errorf("fail to decode return value: %v %x", err, result)
		}
		fmt.Printf("stakeWards.Rewards%s\n", stakeWards.Rewards.BigInt())
		unArrivedRewards = new(big.Int).Add(unArrivedRewards, stakeWards.Rewards.BigInt())
	}
	fmt.Printf("account=%s unArrivedRewards=%s\n", a.Address.String(), unArrivedRewards)

	// get accumulated commission
	commission := big.NewInt(0)
	if a.CheckCommission {
		accumulatedCommission, e := a.getAccumulatedCommission(ctx, checkHeight)
		if e != nil {
			log.Error("getAccumulatedCommission", "err", e)
		}
		commission = accumulatedCommission
		fmt.Printf("account=%s CommissionValidator=%s commission=%s\n", a.Address.String(), a.CommissionValidator, commission)
	}

	arrivedRewards := new(big.Int).Sub(balance, initialBalance)
	arrivedRewards.Add(arrivedRewards, gasFee)
	arrivedRewards.Sub(arrivedRewards, a.NetStake)
	fmt.Printf("account=%s arrivedRewards=%s\n", a.Address.String(), arrivedRewards)

	allRewards := new(big.Int).Add(unArrivedRewards, arrivedRewards)
	if commission != nil {
		allRewards = new(big.Int).Add(allRewards, commission)
	}
	fmt.Printf("account=%s allRewards=%s\n", a.Address.String(), allRewards)

	maxDelta := new(big.Int).Mul(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), big.NewInt(1))

	delta := new(big.Int).Abs(new(big.Int).Sub(allRewards, expectedRewards))
	note = fmt.Sprintf("balance: %s, delta: %s", balance.String(), delta.String())
	fmt.Printf("actionIndex=%d account=%s delta=%s\n", a.Index(), a.Address.String(), delta)
	if delta.Cmp(maxDelta) == 1 {
		return "", fmt.Errorf("actionIndex=%d account: %s balance check failure, allRewards %s, expectedRewards %s, delta %s", a.Index(), a.Address, allRewards, expectedRewards, delta)
	}
	return
}

func (a *CheckBalance) getAccumulatedCommission(ctx *Context, checkHeight uint64) (*big.Int, error) {
	input := &node_manager.GetAccumulatedCommissionParam{ConsensusAddress: a.CommissionValidator}
	data, err := input.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode failed, err: %v", err)
	}
	request := ethereum.CallMsg{To: &NODE_MANAGER_CONTRACT, Data: data}
	output, err := ctx.nodes.Node().CallContract(context.Background(), request, big.NewInt(int64(checkHeight)))
	if err != nil {
		return nil, fmt.Errorf("callContract failed, err: %v", err)
	}
	unpacked, err := node_manager.ABI.Unpack(base.MethodGetAccumulatedCommission, output)
	if err != nil {
		return nil, fmt.Errorf("fail to unpack output: %v %x", err, output)
	}
	result := *abi.ConvertType(unpacked[0], new([]byte)).(*[]byte)
	accumulatedCommission := &node_manager.AccumulatedCommission{}
	err = rlp.DecodeBytes(result, accumulatedCommission)
	if err != nil {
		return nil, fmt.Errorf("fail to decode return value: %v %x", err, result)
	}
	return accumulatedCommission.Amount.BigInt(), nil
}

type GetRewardsReq struct {
	Id        string   `json:"Id"`
	Addresses []string `json:"Addresses"`
	EndHeight uint64   `json:"EndHeight"`
}

type GetRewardsRsp struct {
	Action string `json:"action"`
	Desc   string `json:"desc"`
	Error  int    `json:"error"`
	Result struct {
		Amount []string `json:"Amount"`
		Id     string   `json:"Id"`
	} `json:"result"`
}

type GetGasFeeReq struct {
	Id        string   `json:"Id"`
	Addresses []string `json:"Addresses"`
	EndHeight uint64   `json:"EndHeight"`
}

type GetGasFeeRsp struct {
	Action string `json:"action"`
	Desc   string `json:"desc"`
	Error  int    `json:"error"`
	Result struct {
		Amount []string `json:"Amount"`
		Id     string   `json:"Id"`
	} `json:"result"`
}

func (a *CheckBalance) getExpectedRewards(ctx *Context, address common.Address, height uint64) (*big.Int, error) {
	getRewardsReq := &GetRewardsReq{Addresses: []string{address.String()}, EndHeight: height}
	getRewardsRsp := &GetRewardsRsp{}
	err := PostJsonFor(ctx.getRewardsUrl, getRewardsReq, getRewardsRsp)
	if err != nil || len(getRewardsRsp.Result.Amount) == 0 {
		return nil, fmt.Errorf("getExpectedRewards post failed, err: %v", err)
	}
	expectedRewards, ok := new(big.Int).SetString(getRewardsRsp.Result.Amount[0], 10)
	if !ok {
		return nil, fmt.Errorf("getExpectedRewards convert %s to big.Int failed", getRewardsRsp.Result.Amount[0])
	}
	return expectedRewards, nil
}

func (a *CheckBalance) getGasFee(ctx *Context, address common.Address, height uint64) (*big.Int, error) {
	getGasFeeReq := &GetGasFeeReq{Addresses: []string{address.String()}, EndHeight: height}
	getGasFeeRsp := &GetGasFeeRsp{}
	err := PostJsonFor(ctx.getGasFeeUrl, getGasFeeReq, getGasFeeRsp)
	if err != nil || len(getGasFeeRsp.Result.Amount) == 0 {
		return nil, fmt.Errorf("getGasFees post failed, err: %v", err)
	}
	gasFee, ok := new(big.Int).SetString(getGasFeeRsp.Result.Amount[0], 10)
	if !ok {
		return nil, fmt.Errorf("getExpectedRewards convert %s to big.Int failed", getGasFeeRsp.Result.Amount[0])
	}
	return gasFee, nil
}

func PostJsonFor(url string, payload interface{}, result interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	err = json.Unmarshal(respBody, result)
	if err != nil {
		log.Error("PostJson response", "Body", string(respBody))
	} else {
		log.Debug("PostJson response", "Body", string(respBody))
	}
	return err
}

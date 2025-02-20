package xuperos

import (
	"fmt"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"

	"github.com/xuperchain/xupercore/bcs/ledger/xledger/state"
	"github.com/xuperchain/xupercore/bcs/ledger/xledger/state/xmodel"
	"github.com/xuperchain/xupercore/bcs/ledger/xledger/tx"
	lpb "github.com/xuperchain/xupercore/bcs/ledger/xledger/xldgpb"
	"github.com/xuperchain/xupercore/kernel/common/xaddress"
	xctx "github.com/xuperchain/xupercore/kernel/common/xcontext"
	"github.com/xuperchain/xupercore/kernel/contract"
	"github.com/xuperchain/xupercore/kernel/engines/xuperos/agent"
	"github.com/xuperchain/xupercore/kernel/engines/xuperos/common"
	"github.com/xuperchain/xupercore/kernel/engines/xuperos/miner"
	"github.com/xuperchain/xupercore/kernel/engines/xuperos/parachain"
	"github.com/xuperchain/xupercore/lib/logs"
	"github.com/xuperchain/xupercore/lib/metrics"
	"github.com/xuperchain/xupercore/lib/timer"
	"github.com/xuperchain/xupercore/lib/utils"
	"github.com/xuperchain/xupercore/protos"
)

const (
	// 提交交易cache有效期(s)
	TxIdCacheExpired = 120 * time.Second
	// 提交交易cache GC 周期（s）
	TxIdCacheGCInterval = 180 * time.Second
)

// 定义一条链的具体行为，对外暴露接口错误统一使用标准错误
type Chain struct {
	// 链上下文
	ctx *common.ChainCtx
	// log
	log logs.Logger
	// 矿工
	miner *miner.Miner
	// 依赖代理组件
	relyAgent common.ChainRelyAgent

	// 提交交易cache
	txIdCache *cache.Cache
}

// 从本地存储加载链
func LoadChain(engCtx *common.EngineCtx, bcName string) (*Chain, error) {
	if engCtx == nil || bcName == "" {
		return nil, common.ErrParameter
	}

	// 实例化链日志句柄
	log, err := logs.NewLogger("", bcName)
	if err != nil {
		return nil, common.ErrNewLogFailed
	}

	// 实例化链实例
	ctx := &common.ChainCtx{}
	ctx.EngCtx = engCtx
	ctx.BCName = bcName
	ctx.XLog = log
	ctx.Timer = timer.NewXTimer()
	chainObj := &Chain{}
	chainObj.ctx = ctx
	chainObj.log = ctx.XLog
	chainObj.relyAgent = agent.NewChainRelyAgent(chainObj)

	// 初始化链运行环境上下文
	err = chainObj.initChainCtx()
	if err != nil {
		log.Error("init chain ctx failed", "bcName", bcName, "err", err)
		return nil, common.ErrNewChainCtxFailed.More("err:%v", err)
	}

	// 创建矿工
	chainObj.miner = miner.NewMiner(ctx)
	chainObj.txIdCache = cache.New(TxIdCacheExpired, TxIdCacheGCInterval)

	return chainObj, nil
}

// 供单测时设置rely agent为mock agent，非并发安全
func (t *Chain) SetRelyAgent(agent common.ChainRelyAgent) error {
	if agent == nil {
		return common.ErrParameter
	}

	t.relyAgent = agent
	return nil
}

// 阻塞
func (t *Chain) Start() {
	// 启动矿工
	t.miner.Start()
}

func (t *Chain) Stop() {
	// 停止矿工等其余组件
	t.miner.Stop()
	t.ctx.Ledger.Close()
	t.ctx.State.Close()
	t.ctx = nil
	t.miner = nil
	t.txIdCache = nil
}

func (t *Chain) Context() *common.ChainCtx {
	return t.ctx
}

// 交易预执行
func (t *Chain) PreExec(ctx xctx.XContext, reqs []*protos.InvokeRequest, initiator string, authRequires []string) (*protos.InvokeResponse, error) {
	if ctx == nil || ctx.GetLog() == nil {
		return nil, common.ErrParameter
	}

	reservedRequests, err := t.ctx.State.GetReservedContractRequests(reqs, true)
	if err != nil {
		t.log.Error("PreExec get reserved contract request error", "error", err)
		return nil, common.ErrParameter.More("%v", err)
	}

	transContractName, transAmount, err := tx.ParseContractTransferRequest(reqs)
	if err != nil {
		return nil, common.ErrParameter.More("%v", err)
	}

	reqs = append(reservedRequests, reqs...)
	if len(reqs) <= 0 {
		return &protos.InvokeResponse{}, nil
	}

	stateConfig := &contract.SandboxConfig{
		XMReader:   t.ctx.State.CreateXMReader(),
		UTXOReader: t.ctx.State.CreateUtxoReader(),
	}
	sandbox, err := t.ctx.Contract.NewStateSandbox(stateConfig)
	if err != nil {
		t.log.Error("PreExec new state sandbox error", "error", err)
		return nil, common.ErrContractNewSandboxFailed
	}

	contextConfig := &contract.ContextConfig{
		State:          sandbox,
		Initiator:      initiator,
		AuthRequire:    authRequires,
		ResourceLimits: contract.MaxLimits,
		ChainName:      t.ctx.BCName,
	}

	gasPrice := t.ctx.State.GetMeta().GetGasPrice()
	gasUsed := int64(0)
	responseBodes := make([][]byte, 0, len(reqs))
	requests := make([]*protos.InvokeRequest, 0, len(reqs))
	responses := make([]*protos.ContractResponse, 0, len(reqs))
	for i, req := range reqs {
		if req == nil {
			continue
		}

		if req.ModuleName == "" && req.ContractName == "" && req.MethodName == "" {
			ctx.GetLog().Warn("PreExec req empty", "req", req)
			continue
		}
		if req.ModuleName == "" {
			// 如果请求中不指定 module，根据合约名字查询对应 module。
			// 系统合约仍然需要指定 module，例如部署合约、创建合约账户等，因为系统合约查询不到 module。
			desc, err := t.ctx.State.GetContractDesc(req.ContractName)
			if err != nil {
				return nil, err
			}
			contextConfig.Module = desc.GetContractType()
		} else {
			contextConfig.Module = req.ModuleName
		}

		beginTime := time.Now()

		contextConfig.ContractName = req.ContractName
		if transContractName == req.ContractName {
			contextConfig.TransferAmount = transAmount.String()
		} else {
			contextConfig.TransferAmount = ""
		}

		context, err := t.ctx.Contract.NewContext(contextConfig)
		if err != nil {
			ctx.GetLog().Error("PreExec NewContext error", "error", err, "contractName", req.ContractName)
			if i < len(reservedRequests) && strings.HasSuffix(err.Error(), "not found") {
				requests = append(requests, req)
				continue
			}
			return nil, common.ErrContractNewCtxFailed.More("%v", err)
		}

		resp, err := context.Invoke(req.MethodName, req.Args)
		if err != nil {
			context.Release()
			ctx.GetLog().Error("PreExec Invoke error", "error", err, "contractName", req.ContractName)
			metrics.ContractInvokeCounter.WithLabelValues(t.ctx.BCName, req.ModuleName, req.ContractName, req.MethodName, "InvokeError").Inc()
			return nil, common.ErrContractInvokeFailed.More("%v", err)
		}

		if resp.Status >= 400 && i < len(reservedRequests) {
			context.Release()
			ctx.GetLog().Error("PreExec Invoke error", "status", resp.Status, "contractName", req.ContractName)
			metrics.ContractInvokeCounter.WithLabelValues(t.ctx.BCName, req.ModuleName, req.ContractName, req.MethodName, "InvokeError").Inc()
			return nil, common.ErrContractInvokeFailed.More("%v", resp.Message)
		}

		metrics.ContractInvokeCounter.WithLabelValues(t.ctx.BCName, req.ModuleName, req.ContractName, req.MethodName, "OK").Inc()
		resourceUsed := context.ResourceUsed()
		if i >= len(reservedRequests) {
			gasUsed += resourceUsed.TotalGas(gasPrice)
		}

		// request
		request := *req
		request.ResourceLimits = contract.ToPbLimits(resourceUsed)
		requests = append(requests, &request)

		// response
		response := &protos.ContractResponse{
			Status:  int32(resp.Status),
			Message: resp.Message,
			Body:    resp.Body,
		}
		responses = append(responses, response)
		responseBodes = append(responseBodes, resp.Body)

		context.Release()
		metrics.ContractInvokeHistogram.WithLabelValues(t.ctx.BCName, req.ModuleName, req.ContractName, req.MethodName).Observe(time.Since(beginTime).Seconds())
	}

	err = sandbox.Flush()
	if err != nil {
		return nil, err
	}
	rwSet := sandbox.RWSet()
	utxoRWSet := sandbox.UTXORWSet()

	invokeResponse := &protos.InvokeResponse{
		GasUsed:     gasUsed,
		Response:    responseBodes,
		Inputs:      xmodel.GetTxInputs(rwSet.RSet),
		Outputs:     xmodel.GetTxOutputs(rwSet.WSet),
		Requests:    requests,
		Responses:   responses,
		UtxoInputs:  utxoRWSet.Rset,
		UtxoOutputs: utxoRWSet.WSet,
	}

	return invokeResponse, nil
}

// 提交交易到交易池(xuperos引擎同时更新到状态机和交易池)
func (t *Chain) SubmitTx(ctx xctx.XContext, tx *lpb.Transaction) error {
	if tx == nil || ctx == nil || ctx.GetLog() == nil || len(tx.GetTxid()) <= 0 {
		return common.ErrParameter
	}
	log := ctx.GetLog()

	// 无币化
	if len(tx.TxInputs) == 0 && !t.ctx.Ledger.GetNoFee() {
		ctx.GetLog().Warn("PostTx TxInputs can not be null while need utxo")
		return common.ErrTxNotEnough
	}

	// 防止重复提交交易
	if _, exist := t.txIdCache.Get(string(tx.GetTxid())); exist {
		return common.ErrTxAlreadyExist
	}
	t.txIdCache.Set(string(tx.GetTxid()), true, TxIdCacheExpired)

	code := "OK"
	defer func() {
		metrics.CallMethodCounter.WithLabelValues(t.ctx.BCName, "SubmitTx", code).Inc()
	}()

	// 判断此交易是否已经存在（账本和未确认交易表中）。
	dbtx, _, _ := t.ctx.State.QueryTx(tx.GetTxid())
	if dbtx != nil { // 从数据库查询到了交易，返回错误。
		log.Error("tx already exist", "txid", utils.F(tx.GetTxid()))
		return common.ErrTxAlreadyExist
	}

	// 验证交易
	_, err := t.ctx.State.VerifyTx(tx)
	if err != nil {
		log.Error("verify tx error", "txid", utils.F(tx.GetTxid()), "err", err)
		code = "VerifyTxFailed"
		return common.ErrTxVerifyFailed.More("err:%v", err)
	}

	// 提交交易
	err = t.ctx.State.DoTx(tx)
	if err != nil {
		log.Error("submit tx error", "txid", utils.F(tx.GetTxid()), "err", err)
		if err == state.ErrAlreadyInUnconfirmed {
			t.txIdCache.Delete(string(tx.GetTxid()))
		}
		code = "SubmitTxFailed"
		return common.ErrSubmitTxFailed.More("err:%v", err)
	}

	return nil
}

// 处理P2P网络同步到的区块
func (t *Chain) ProcBlock(ctx xctx.XContext, block *lpb.InternalBlock) error {
	if block == nil || ctx == nil || ctx.GetLog() == nil || block.GetBlockid() == nil {
		return common.ErrParameter
	}

	log := ctx.GetLog()
	err := t.miner.ProcBlock(ctx, block)
	if err != nil {
		if common.CastError(err).Equal(common.ErrForbidden) {
			log.Trace("forbidden process block", "blockid", utils.F(block.GetBlockid()), "err", err)
			return common.ErrForbidden
		}

		if common.CastError(err).Equal(common.ErrParameter) {
			log.Trace("param error")
			return common.ErrParameter
		}

		ctx.GetLog().Warn("process block failed", "blockid", utils.F(block.GetBlockid()), "err", err)
		return common.ErrProcBlockFailed.More("err:%v", err)
	}

	log.Info("process block succ", "height", block.GetHeight(), "blockid", utils.F(block.GetBlockid()))
	return nil
}

// 初始化链运行依赖上下文
func (t *Chain) initChainCtx() error {
	// 1.实例化账本
	leg, err := t.relyAgent.CreateLedger()
	if err != nil {
		t.log.Error("open ledger failed", "bcName", t.ctx.BCName, "err", err)
		return err
	}
	t.ctx.Ledger = leg
	t.log.Trace("open ledger succ", "bcName", t.ctx.BCName)

	// 2.实例化加密组件
	// 从账本查询加密算法类型
	cryptoType, err := agent.NewLedgerAgent(t.ctx).GetCryptoType()
	if err != nil {
		t.log.Error("query crypto type failed", "bcName", t.ctx.BCName, "err", err)
		return fmt.Errorf("query crypto type failed")
	}
	crypt, err := t.relyAgent.CreateCrypto(cryptoType)
	if err != nil {
		t.log.Error("create crypto client failed", "error", err)
		return fmt.Errorf("create crypto client failed")
	}
	t.ctx.Crypto = crypt
	t.log.Trace("create crypto client succ", "bcName", t.ctx.BCName, "cryptoType", cryptoType)

	// 3.实例化状态机
	stat, err := t.relyAgent.CreateState(leg, crypt)
	if err != nil {
		t.log.Error("open state failed", "bcName", t.ctx.BCName, "err", err)
		return fmt.Errorf("open state failed")
	}
	t.ctx.State = stat
	t.log.Trace("open state succ", "bcName", t.ctx.BCName)

	// 4.加载节点账户信息
	keyPath := t.ctx.EngCtx.EnvCfg.GenDataAbsPath(t.ctx.EngCtx.EnvCfg.KeyDir)
	addr, err := xaddress.LoadAddrInfo(keyPath, t.ctx.Crypto)
	if err != nil {
		t.log.Error("load node addr info error", "bcName", t.ctx.BCName, "keyPath", keyPath, "err", err)
		return fmt.Errorf("load node addr info error")
	}
	t.ctx.Address = addr
	t.log.Trace("load node addr info succ", "bcName", t.ctx.BCName, "address", addr.Address)

	// 5.合约
	contractObj, err := t.relyAgent.CreateContract(stat.CreateXMReader())
	if err != nil {
		t.log.Error("create contract manager error", "bcName", t.ctx.BCName, "err", err)
		return fmt.Errorf("create contract manager error")
	}
	t.ctx.Contract = contractObj
	// 设置合约manager到状态机
	t.ctx.State.SetContractMG(t.ctx.Contract)
	t.log.Trace("create contract manager succ", "bcName", t.ctx.BCName)

	// 6.Acl
	aclObj, err := t.relyAgent.CreateAcl()
	if err != nil {
		t.log.Error("create acl error", "bcName", t.ctx.BCName, "err", err)
		return fmt.Errorf("create acl error")
	}
	t.ctx.Acl = aclObj
	// 设置acl manager到状态机
	t.ctx.State.SetAclMG(t.ctx.Acl)
	t.log.Trace("create acl succ", "bcName", t.ctx.BCName)

	// 7.共识
	cons, err := t.relyAgent.CreateConsensus()
	if err != nil {
		t.log.Error("create consensus error", "bcName", t.ctx.BCName, "err", err)
		return fmt.Errorf("create consensus error")
	}
	t.ctx.Consensus = cons
	t.log.Trace("create consensus succ", "bcName", t.ctx.BCName)

	// 8.提案
	governTokenObj, err := t.relyAgent.CreateGovernToken()
	if err != nil {
		t.log.Error("create govern token error", "bcName", t.ctx.BCName, "err", err)
		return fmt.Errorf("create govern token error")
	}
	t.ctx.GovernToken = governTokenObj
	// 设置govern token manager到状态机
	t.ctx.State.SetGovernTokenMG(t.ctx.GovernToken)
	t.log.Trace("create govern token succ", "bcName", t.ctx.BCName)

	// 9.提案
	proposalObj, err := t.relyAgent.CreateProposal()
	if err != nil {
		t.log.Error("create proposal error", "bcName", t.ctx.BCName, "err", err)
		return fmt.Errorf("create proposal error")
	}
	t.ctx.Proposal = proposalObj
	// 设置proposal manager到状态机
	t.ctx.State.SetProposalMG(t.ctx.Proposal)
	t.log.Trace("create proposal succ", "bcName", t.ctx.BCName)

	// 10.定时器任务
	timerObj, err := t.relyAgent.CreateTimerTask()
	if err != nil {
		t.log.Error("create timer_task error", "bcName", t.ctx.BCName, "err", err)
		return fmt.Errorf("create timer_task error")
	}
	t.ctx.TimerTask = timerObj
	// 设置timer manager到状态机
	t.ctx.State.SetTimerTaskMG(t.ctx.TimerTask)
	t.log.Trace("create timer_task succ", "bcName", t.ctx.BCName)
	t.log.Trace("create chain succ", "bcName", t.ctx.BCName)

	// 11.xtoken创建
	_, err = t.relyAgent.CreateXToken()
	if err != nil {
		return err
	}
	t.log.Trace("create xtoken succ", "bcName", t.ctx.BCName)
	return nil
}

// 创建平行链实例
func (t *Chain) CreateParaChain() error {
	paraChainCtx, err := parachain.NewParaChainCtx(t.ctx.BCName, t.ctx)
	if err != nil {
		return fmt.Errorf("create parachain ctx failed.err:%v", err)
	}
	_, err = parachain.NewParaChainManager(paraChainCtx)
	if err != nil {
		return fmt.Errorf("create parachain instance failed.err:%v", err)
	}
	return nil
}

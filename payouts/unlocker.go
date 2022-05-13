package payouts

import (
	"fmt"
	"github.com/cellcrypto/open-dangnn-pool/hook"
	"github.com/cellcrypto/open-dangnn-pool/storage/mysql"
	"github.com/cellcrypto/open-dangnn-pool/storage/redis"
	"github.com/cellcrypto/open-dangnn-pool/storage/types"
	"github.com/cellcrypto/open-dangnn-pool/util/plogger"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/cellcrypto/open-dangnn-pool/rpc"
	"github.com/cellcrypto/open-dangnn-pool/util"
)

type UnlockerConfig struct {
	Enabled        bool    `json:"enabled"`
	PoolFee        float64 `json:"poolFee"`
	PoolFeeAddress string  `json:"poolFeeAddress"`
	Donate         bool    `json:"donate"`
	Depth          int64   `json:"depth"`
	ImmatureDepth  int64   `json:"immatureDepth"`
	KeepTxFees     bool    `json:"keepTxFees"`
	Interval       string  `json:"interval"`
	Daemon         string  `json:"daemon"`
	Timeout        string  `json:"timeout"`
}

const minDepth = 16
const byzantiumHardForkHeight = 0

//var GenesisReword =   math.MustParseBig256("300000000000000000000")
//var byzantiumReward = math.MustParseBig256("300000000000000000000")

// Donate 10% from pool fees to developers
const donationFee = 10.0
const donationAccount = "0xb05146ed865f0ab592dd763bd84a2191700f3dfb"

type BlockUnlocker struct {
	config   *UnlockerConfig
	backend  *redis.RedisClient
	db 		 *mysql.Database
	rpc      *rpc.RPCClient
	halt     bool
	lastFail error
	mainNet  bool
}

func NewBlockUnlocker(cfg *UnlockerConfig, backend *redis.RedisClient, db *mysql.Database, mainnet string, netId int64) *BlockUnlocker {
	if len(cfg.PoolFeeAddress) != 0 && !util.IsValidHexAddress(cfg.PoolFeeAddress) {
		log.Fatalln("Invalid poolFeeAddress", cfg.PoolFeeAddress)
	}
	if cfg.Depth < minDepth*2 {
		log.Fatalf("Block maturity depth can't be < %v, your depth is %v", minDepth*2, cfg.Depth)
	}
	if cfg.ImmatureDepth < minDepth {
		log.Fatalf("Immature depth can't be < %v, your depth is %v", minDepth, cfg.ImmatureDepth)
	}
	net := true
	if mainnet != "testnet" {
		net = true
	} else {
		net = false
	}

	u := &BlockUnlocker{
		config: cfg,
		backend: backend,
		db: db,
		mainNet: net,
	}
	u.rpc = rpc.NewRPCClient("BlockUnlocker", cfg.Daemon, cfg.Timeout, netId)
	return u
}

func (u *BlockUnlocker) Start() {
	log.Println("Starting block unlocker")
	intv := util.MustParseDuration(u.config.Interval)
	timer := time.NewTimer(intv)
	log.Printf("Set block unlock interval to %v", intv)

	// Immediately unlock after start
	u.unlockPendingBlocks()
	u.unlockAndCreditMiners()
	timer.Reset(intv)
	quit := make(chan struct{})
	hooks := make(chan struct{})

	plogger.InsertLog("START UNLOCK SERVER", plogger.LogTypeSystem, plogger.LogErrorNothing, 0, 0, "", "")
	hook.RegistryHook("unlock.go", func(name string) {
		plogger.InsertLog("SHUTDOWN UNLOCK SERVER", plogger.LogTypeSystem, plogger.LogErrorNothing, 0, 0, "", "")
		close(quit)
		<- hooks
	})

	go func() {
		for {
			select {
			case <-quit:
				hooks <- struct{}{}
				return
			case <-timer.C:
				u.unlockPendingBlocks()
				u.unlockAndCreditMiners()
				timer.Reset(intv)
			}
		}
	}()
}

type UnlockResult struct {
	maturedBlocks  []*types.BlockData
	orphanedBlocks []*types.BlockData
	orphans        int
	uncles         int
	blocks         int
}

/* Geth does not provide consistent state when you need both new height and new job,
 * so in redis I am logging just what I have in a pool state on the moment when block found.
 * Having very likely incorrect height in database results in a weird block unlocking scheme,
 * when I have to check what the hell we actually found and traversing all the blocks with height-N and height+N
 * to make sure we will find it. We can't rely on round height here, it's just a reference point.
 * ISSUE: https://github.com/ethereum/go-ethereum/issues/2333
 */
func (u *BlockUnlocker) unlockCandidates(candidates []*types.BlockData) (*UnlockResult, error) {
	result := &UnlockResult{}

	// Data row is: "height:nonce:powHash:mixDigest:timestamp:diff:totalShares"
	for _, candidate := range candidates {
		orphan := true

		/* Search for a normal block with wrong height here by traversing 16 blocks back and forward.
		 * Also we are searching for a block that can include this one as uncle.
		 */
		for i := int64(minDepth * -1); i < minDepth; i++ {
			height := candidate.Height + i

			if height < 0 {
				continue
			}

			block, err := u.rpc.GetBlockByHeight(height)
			if err != nil {
				log.Printf("Error while retrieving block %v from node: %v", height, err)
				return nil, err
			}
			if block == nil {
				return nil, fmt.Errorf("Error while retrieving block %v from node, wrong node height", height)
			}

			if matchCandidate(block, candidate) {
				orphan = false
				result.blocks++

				err = u.handleBlock(block, candidate)
				if err != nil {
					u.halt = true
					u.lastFail = err
					return nil, err
				}
				result.maturedBlocks = append(result.maturedBlocks, candidate)
				log.Printf("Mature block %v with %v tx, hash: %v", candidate.Height, len(block.Transactions), candidate.Hash[0:10])
				break
			}

			if len(block.Uncles) == 0 {
				continue
			}

			// Trying to find uncle in current block during our forward check
			for uncleIndex, uncleHash := range block.Uncles {
				uncle, err := u.rpc.GetUncleByBlockNumberAndIndex(height, uncleIndex)
				if err != nil {
					return nil, fmt.Errorf("Error while retrieving uncle of block %v from node: %v", uncleHash, err)
				}
				if uncle == nil {
					return nil, fmt.Errorf("Error while retrieving uncle of block %v from node", height)
				}

				// Found uncle
				if matchCandidate(uncle, candidate) {
					orphan = false
					result.uncles++

					err := u.handleUncle(height, uncle, candidate)
					if err != nil {
						u.halt = true
						u.lastFail = err
						return nil, err
					}
					result.maturedBlocks = append(result.maturedBlocks, candidate)
					log.Printf("Mature uncle %v/%v of reward %v with hash: %v", candidate.Height, candidate.UncleHeight,
						util.FormatReward(candidate.Reward), uncle.Hash[0:10])
					break
				}
			}
			// Found block or uncle
			if !orphan {
				break
			}
		}
		// Block is lost, we didn't find any valid block or uncle matching our data in a blockchain
		if orphan {
			result.orphans++
			candidate.Orphan = true
			result.orphanedBlocks = append(result.orphanedBlocks, candidate)
			log.Printf("Orphaned block %v:%v", candidate.RoundHeight, candidate.Nonce)
		}
	}
	return result, nil
}

func matchCandidate(block *rpc.GetBlockReply, candidate *types.BlockData) bool {
	// Just compare hash if block is unlocked as immature
	if len(candidate.Hash) > 0 && strings.EqualFold(candidate.Hash, block.Hash) {
		return true
	}
	// Geth-style candidate matching
	if len(block.Nonce) > 0 {
		return strings.EqualFold(block.Nonce, candidate.Nonce)
	}
	// Parity's EIP: https://github.com/ethereum/EIPs/issues/95
	if len(block.SealFields) == 2 {
		return strings.EqualFold(candidate.Nonce, block.SealFields[1])
	}
	return false
}

func (u *BlockUnlocker) handleBlock(block *rpc.GetBlockReply, candidate *types.BlockData) error {
	correctHeight, err := strconv.ParseInt(strings.Replace(block.Number, "0x", "", -1), 16, 64)
	if err != nil {
		return err
	}
	candidate.Height = correctHeight
	reward := types.GetConstReward(candidate.Height, u.mainNet)

	// Add TX fees
	extraTxReward, err := u.getExtraRewardForTx(block)
	if err != nil {
		return fmt.Errorf("Error while fetching TX receipt: %v", err)
	}
	if u.config.KeepTxFees {
		candidate.ExtraReward = extraTxReward
	} else {
		reward.Add(reward, extraTxReward)
	}

	// Add reward for including uncles
	uncleReward := types.GetRewardForUncle(candidate.Height, u.mainNet)
	rewardForUncles := big.NewInt(0).Mul(uncleReward, big.NewInt(int64(len(block.Uncles))))
	reward.Add(reward, rewardForUncles)

	candidate.Orphan = false
	candidate.Hash = block.Hash
	candidate.Reward = reward
	return nil
}

func (u *BlockUnlocker) handleUncle(height int64, uncle *rpc.GetBlockReply, candidate *types.BlockData) error {
	uncleHeight, err := strconv.ParseInt(strings.Replace(uncle.Number, "0x", "", -1), 16, 64)
	if err != nil {
		return err
	}
	reward := types.GetUncleReward(uncleHeight, height, u.mainNet)
	if reward.Cmp(big.NewInt(0)) < 0 {
		reward = big.NewInt(0)
	}
	candidate.Height = height
	candidate.UncleHeight = uncleHeight
	candidate.Orphan = false
	candidate.Hash = uncle.Hash
	candidate.Reward = reward
	return nil
}

func (u *BlockUnlocker) unlockPendingBlocks() {
	if u.halt {
		log.Println("Unlocking suspended due to last critical error:", u.lastFail)
		return
	}

	current, err := u.rpc.GetPendingBlock()
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Unable to get current blockchain height from node: %v", err)
		plogger.InsertSystemError(plogger.LogTypePendingBlock, 0, 0, "Unable to get current blockchain height from node: %v", err)
		return
	}
	currentHeight, err := strconv.ParseInt(strings.Replace(current.Number, "0x", "", -1), 16, 64)
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Can't parse pending block number: %v", err)
		plogger.InsertSystemError(plogger.LogTypePendingBlock, 0, 0, "Can't parse pending block number: %v", err)
		return
	}

	candidates, err := u.db.GetCandidates(currentHeight - u.config.ImmatureDepth)
	//candidates, err := u.backend.GetCandidates(currentHeight - u.config.ImmatureDepth)
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Failed to get block candidates from backend: %v", err)
		plogger.InsertSystemError(plogger.LogTypePendingBlock, 0, 0, "Failed to get block candidates from backend: %v", err)
		return
	}

	if len(candidates) == 0 {
		log.Println("[Info] No block candidates to unlock")
		return
	}

	result, err := u.unlockCandidates(candidates)
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Failed to unlock blocks: %v", err)
		plogger.InsertSystemError(plogger.LogTypePendingBlock, 0, 0, "Failed to unlock blocks: %v", err)
		return
	}
	log.Printf("Immature %v blocks, %v uncles, %v orphans", result.blocks, result.uncles, result.orphans)

	err = u.db.WritePendingOrphans(result.orphanedBlocks)
	//err = u.backend.WritePendingOrphans(result.orphanedBlocks)
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Failed to insert orphaned blocks into backend: %v", err)
		plogger.InsertSystemError(plogger.LogTypePendingBlock, 0, 0, "Failed to insert orphaned blocks into backend: %v", err)
		return
	} else {
		log.Printf("Inserted %v orphaned blocks to backend", result.orphans)
	}

	totalRevenue := new(big.Rat)
	totalMinersProfit := new(big.Rat)
	totalPoolProfit := new(big.Rat)

	start := time.Now()
	for _, block := range result.maturedBlocks {
		revenue, minersProfit, poolProfit, roundRewards, percents, err := u.calculateRewards(block)
		if err != nil {
			u.halt = true
			u.lastFail = err
			//log.Printf("Failed to calculate rewards for round %v: %v", block.RoundKey(), err)
			plogger.InsertSystemError(plogger.LogTypePendingBlock, block.RoundHeight, block.Height, "Failed to calculate rewards for round %v: %v", block.RoundKey(), err)
			return
		}

		if roundRewards == nil {
			// If the list to receive the reward is not listed in Redis.
			u.db.WriteImmatureError(block, 0, 1)
			plogger.InsertLog("Failure: Redis has no one to share the rewards with", plogger.LogTypePendingBlock, plogger.LogErrorNothingRoundBlock, block.RoundHeight, block.Height,"", "")
			continue
		}

		totalRevenue.Add(totalRevenue, revenue)
		totalMinersProfit.Add(totalMinersProfit, minersProfit)
		totalPoolProfit.Add(totalPoolProfit, poolProfit)

		var hashName string
		if block.UncleHeight > 0 {
			hashName = util.Join(fmt.Sprintf("uncle(%v)", block.Height - block.UncleHeight),block.UncleHeight,block.Hash)
		} else {
			hashName = util.Join(block.Height,block.Hash)
		}

		logEntry := fmt.Sprintf(
			"IMMATURE %v: size: %d,revenue %v, miners profit %v, pool profit: %v",
			hashName,
			len(roundRewards),
			util.FormatRatReward(revenue),
			util.FormatRatReward(minersProfit),
			util.FormatRatReward(poolProfit),
		)

		err = u.db.WriteImmatureBlock(block, roundRewards, percents)
		//err = u.backend.WriteImmatureBlock(block, roundRewards)
		if err != nil {
			u.halt = true
			u.lastFail = err
			//log.Printf("Failed to credit rewards for round %v: %v", block.RoundKey(), err)
			plogger.InsertSystemError(plogger.LogTypePendingBlock, block.RoundHeight, block.Height, "Failed to credit rewards for round %v: %v", block.RoundKey(), err)
			return
		}

		plogger.InsertLog(logEntry, plogger.LogTypePendingBlock, plogger.LogErrorNothing, block.RoundHeight, block.Height,"", "")

		log.Println(logEntry)
	}

	log.Printf(
		"(%v) IMMATURE SESSION: block size: %v,revenue %v, miners profit %v, pool profit: %v",
		time.Since(start),
		len(result.maturedBlocks),
		util.FormatRatReward(totalRevenue),
		util.FormatRatReward(totalMinersProfit),
		util.FormatRatReward(totalPoolProfit),
	)
}

func (u *BlockUnlocker) unlockAndCreditMiners() {
	if u.halt {
		log.Println("unlockAndCreditMiners: Unlocking suspended due to last critical error:", u.lastFail)
		return
	}

	current, err := u.rpc.GetPendingBlock()
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Unable to get current blockchain height from node: %v", err)
		plogger.InsertSystemError(plogger.LogTypeMaturedBlock, 0, 0, "Unable to get current blockchain height from node: %v", err)
		return
	}
	currentHeight, err := strconv.ParseInt(strings.Replace(current.Number, "0x", "", -1), 16, 64)
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Can't parse pending block number: %v", err)
		plogger.InsertSystemError(plogger.LogTypeMaturedBlock, 0, 0, "Can't parse pending block number: %v", err)
		return
	}

	immature, err := u.db.GetImmatureBlocks(currentHeight - u.config.Depth)
	//immature, err := u.backend.GetImmatureBlocks(currentHeight - u.config.Depth)
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Failed to get block candidates from backend: %v", err)
		plogger.InsertSystemError(plogger.LogTypeMaturedBlock, 0, 0, "Failed to get block candidates from backend: %v", err)
		return
	}

	if len(immature) == 0 {
		log.Println("[Info] No immature blocks to credit miners")
		return
	}

	result, err := u.unlockCandidates(immature)
	if err != nil {
		u.halt = true
		u.lastFail = err
		//log.Printf("Failed to unlock blocks: %v", err)
		plogger.InsertSystemError(plogger.LogTypeMaturedBlock, 0, 0, "Failed to unlock blocks: %v", err)
		return
	}
	log.Printf("Unlocked %v blocks, %v uncles, %v orphans", result.blocks, result.uncles, result.orphans)

	for _, block := range result.orphanedBlocks {
		err = u.db.WriteOrphan(block)
		// err = u.backend.WriteOrphan(block)
		if err != nil {
			u.halt = true
			u.lastFail = err
			// log.Printf("Failed to insert orphaned block into backend: %v", err)
			plogger.InsertSystemError(plogger.LogTypeMaturedBlock, block.RoundHeight, block.Height, "Failed to insert orphaned block into backend: %v", err)
			return
		}
	}
	log.Printf("Inserted %v orphaned blocks to backend", result.orphans)

	totalRevenue := new(big.Rat)
	totalMinersProfit := new(big.Rat)
	totalPoolProfit := new(big.Rat)

	start := time.Now()

	for _, block := range result.maturedBlocks {
		revenue, minersProfit, poolProfit, roundRewards, percents, err := u.calculateRewards(block)
		if err != nil {
			u.halt = true
			u.lastFail = err
			//log.Printf("Failed to calculate rewards for round %v: %v", block.RoundKey(), err)
			plogger.InsertSystemError(plogger.LogTypeMaturedBlock, block.RoundHeight, block.Height, "Failed to calculate rewards for round %v: %v", block.RoundKey(), err)
			return
		}

		if roundRewards == nil {
			// If the list to receive the reward is not listed in Redis.
			u.db.WriteImmatureError(block, block.State, 2)
			log.Printf("Failed: No round_block information for reward in Redis.")
			plogger.InsertLog("Failed: No round_block information for reward in Redis.",
				plogger.LogTypeMaturedBlock,plogger.LogSubTypeSystemRoundInfoRedis, block.RoundHeight, block.Height, "", "")
			continue
		}

		err = u.db.WriteMaturedBlock(block, roundRewards, percents)
		// err = u.backend.WriteMaturedBlock(block, roundRewards)
		if err != nil {
			u.halt = true
			u.lastFail = err
			//log.Printf("Failed to credit rewards for round %v: %v", block.RoundKey(), err)
			plogger.InsertSystemError(plogger.LogTypeMaturedBlock, block.RoundHeight, block.Height, "Failed to credit rewards for round %v: %v", block.RoundKey(), err)
			return
		}

		totalRevenue.Add(totalRevenue, revenue)
		totalMinersProfit.Add(totalMinersProfit, minersProfit)
		totalPoolProfit.Add(totalPoolProfit, poolProfit)

		logEntry := fmt.Sprintf(
			"MATURED %v: size %v,revenue %v, miners profit %v, pool profit: %v",
			block.RoundKey(),
			len(roundRewards),
			util.FormatRatReward(revenue),
			util.FormatRatReward(minersProfit),
			util.FormatRatReward(poolProfit),
		)

		plogger.InsertLog(logEntry, plogger.LogTypeMaturedBlock, plogger.LogErrorNothing, block.RoundHeight, block.Height,"", "")

		log.Println(logEntry)
	}

	log.Printf(
		"(%s) MATURE SESSION: block size: %v,revenue %v, miners profit %v, pool profit: %v",
		time.Since(start),
		len(result.maturedBlocks),
		util.FormatRatReward(totalRevenue),
		util.FormatRatReward(totalMinersProfit),
		util.FormatRatReward(totalPoolProfit),
	)
}

func (u *BlockUnlocker) calculateRewards(block *types.BlockData) (*big.Rat, *big.Rat, *big.Rat, map[string]int64, map[string]*big.Rat, error) {
	revenue := new(big.Rat).SetInt(block.Reward)
	minersProfit, poolProfit := chargeFee(revenue, u.config.PoolFee)

	shares, err := u.backend.GetRoundShares(block.RoundHeight, block.Nonce)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	// shares are not in Redis.
	if len(shares) == 0 {
		return nil, nil, nil, nil, nil, nil
	}

	totalShares := int64(0)
	for _, val := range shares {
		totalShares += val
	}

	rewards, percents := calculateRewardsForShares(shares, totalShares, minersProfit)

	if block.ExtraReward != nil {
		extraReward := new(big.Rat).SetInt(block.ExtraReward)
		poolProfit.Add(poolProfit, extraReward)
		revenue.Add(revenue, extraReward)
	}

	if u.config.Donate {
		var donation = new(big.Rat)
		poolProfit, donation = chargeFee(poolProfit, donationFee)
		login := strings.ToLower(donationAccount)
		rewards[login] += weiToShannonInt64(donation)
	}

	if len(u.config.PoolFeeAddress) != 0 {
		address := strings.ToLower(u.config.PoolFeeAddress)
		rewards[address] += weiToShannonInt64(poolProfit)
	}

	return revenue, minersProfit, poolProfit, rewards, percents, nil
}

func calculateRewardsForShares(shares map[string]int64, total int64, reward *big.Rat) (map[string]int64, map[string]*big.Rat) {
	rewards := make(map[string]int64)
	percents := make(map[string]*big.Rat)

	for login, n := range shares {
		percents[login] = big.NewRat(n, total)
		workerReward := new(big.Rat).Mul(reward, percents[login])
		rewards[login] += weiToShannonInt64(workerReward)
	}
	return rewards, percents
}

// Returns new value after fee deduction and fee value.
func chargeFee(value *big.Rat, fee float64) (*big.Rat, *big.Rat) {
	feePercent := new(big.Rat).SetFloat64(fee / 100)
	feeValue := new(big.Rat).Mul(value, feePercent)
	return new(big.Rat).Sub(value, feeValue), feeValue
}

func weiToShannonInt64(wei *big.Rat) int64 {
	shannon := new(big.Rat).SetInt(util.Shannon)
	inShannon := new(big.Rat).Quo(wei, shannon)
	value, _ := strconv.ParseInt(inShannon.FloatString(0), 10, 64)
	return value
}


func (u *BlockUnlocker) getExtraRewardForTx(block *rpc.GetBlockReply) (*big.Int, error) {
	amount := new(big.Int)

	for _, tx := range block.Transactions {
		receipt, err := u.rpc.GetTxReceipt(tx.Hash)
		if err != nil {
			return nil, err
		}
		if receipt != nil {
			gasUsed := util.String2Big(receipt.GasUsed)
			gasPrice := util.String2Big(tx.GasPrice)
			fee := new(big.Int).Mul(gasUsed, gasPrice)
			amount.Add(amount, fee)
		}
	}
	return amount, nil
}

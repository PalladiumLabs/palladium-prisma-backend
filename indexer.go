package main

import (
	"context"
	"log"
	"math"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ------------------ Mongo Structs ------------------

type HistoryEntry struct {
	TxHash      string  `bson:"txHash"`
	Coll        float64 `bson:"coll"`
	Debt        float64 `bson:"debt"`
	TxType      string  `bson:"txType"`
	Timestamp   string  `bson:"timestamp"`
	BlockNumber uint64  `bson:"blockNumber"`
}

type Position struct {
	PositionID    int            `bson:"positionID"`
	WalletAddress string         `bson:"walletAddress"`
	Asset         string         `bson:"asset"`
	Coll          float64        `bson:"coll"`
	Debt          float64        `bson:"debt"`
	Status        string         `bson:"status"`
	Nltv          float64        `bson:"nltv"`
	BlockNumber   uint64         `bson:"blockNumber"`
	History       []HistoryEntry `bson:"history"`
}

type ContractEvent struct {
	Name     string
	Data     map[string]interface{}
	RawLog   types.Log
	Contract string
}

var (
	mongoClient  *mongo.Client
	positionColl *mongo.Collection
	historyColl  *mongo.Collection
)

// ------------------ Helpers ------------------

func loadABI(filename string) string {
	data, err := os.ReadFile(filename)
	if err != nil {
		log.Fatalf("❌ Failed to read ABI file %s: %v", filename, err)
	}
	return string(data)
}

func bigIntToFloat(val *big.Int) float64 {
	if val == nil {
		return 0
	}
	f := new(big.Float).SetInt(val)
	div := big.NewFloat(1e18)
	result, _ := new(big.Float).Quo(f, div).Float64()
	return result
}

func calculateNLTV(debt, collateral float64) float64 {
	if collateral == 0 {
		return 0
	}
	return math.Round((debt/collateral)*10000) / 100
}

func getTxType(op uint8) string {
	switch op {
	case 0:
		return "openTrove"
	case 1:
		return "closeTrove"
	case 2:
		return "adjustTrove"
	default:
		return "unknown"
	}
}

func getNextPositionID() int {
	opts := options.FindOne().SetSort(bson.D{{Key: "positionID", Value: -1}})
	var last Position
	err := positionColl.FindOne(context.TODO(), bson.D{}, opts).Decode(&last)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 1
		}
		log.Fatalf("failed to fetch latest positionID: %v", err)
	}
	return last.PositionID + 1
}

// ------------------ Position Updater ------------------

func handlePositionUpdate(wallet, asset, op string, collateral, debt float64, history HistoryEntry, blockNumber uint64) error {
	if op == "openTrove" {
		positionID := getNextPositionID()
		doc := bson.D{
			{Key: "positionID", Value: positionID},
			{Key: "walletAddress", Value: wallet},
			{Key: "asset", Value: asset},
			{Key: "coll", Value: collateral},
			{Key: "debt", Value: debt},
			{Key: "nltv", Value: calculateNLTV(debt, collateral)},
			{Key: "status", Value: "active"},
			{Key: "blockNumber", Value: blockNumber},
			{Key: "history", Value: bson.A{history}},
		}

		_, err := positionColl.InsertOne(context.TODO(), doc)
		if err != nil {
			return err
		}
		return nil
	}

	// Update existing position
	filter := bson.M{"walletAddress": wallet, "asset": asset, "status": "active"}
	var latest Position
	err := positionColl.FindOne(context.TODO(), filter).Decode(&latest)
	if err != nil {
		return err
	}

	status := "active"
	if op == "closeTrove" {
		status = "closed"
	} else if debt == 0 {
		status = "liquidated"
	}

	update := bson.M{
		"$set": bson.M{
			"coll":        collateral,
			"debt":        debt,
			"nltv":        calculateNLTV(debt, collateral),
			"status":      status,
			"blockNumber": blockNumber,
		},
		"$push": bson.M{"history": history},
	}

	_, err = positionColl.UpdateOne(context.TODO(), bson.M{"positionID": latest.PositionID}, update)
	return err
}

// ------------------ Event Handler ------------------

func handleEvent(ev ContractEvent, troveABI abi.ABI) {
	log.Printf("📢 Event: %s | Contract: %s | TxHash: %s", ev.Name, ev.Contract, ev.RawLog.TxHash.Hex())

	// Save raw history
	doc := map[string]interface{}{
		"event":        ev.Name,
		"contract":     ev.Contract,
		"txHash":       ev.RawLog.TxHash.Hex(),
		"blockNumber":  ev.RawLog.BlockNumber,
		"decoded_data": ev.Data,
		"timestamp":    time.Now(),
	}
	_, _ = historyColl.InsertOne(context.Background(), doc)

	// Handle TroveUpdated → update positions
	if ev.Name == "TroveUpdated" {
		coll := bigIntToFloat(ev.Data["_coll"].(*big.Int))
		debt := bigIntToFloat(ev.Data["_debt"].(*big.Int))
		op := getTxType(ev.Data["_operation"].(uint8))

		// 🛠 FIX: safe handling of topics
		var wallet, asset string
		if len(ev.RawLog.Topics) > 1 {
			wallet = strings.ToLower(common.HexToAddress(ev.RawLog.Topics[1].Hex()).Hex())
		}
		if len(ev.RawLog.Topics) > 2 {
			asset = strings.ToLower(common.HexToAddress(ev.RawLog.Topics[2].Hex()).Hex())
		}

		log.Printf("Decoded TroveUpdated → wallet=%s | asset=%s | coll=%.4f | debt=%.4f | op=%s | topics=%d",
			wallet, asset, coll, debt, op, len(ev.RawLog.Topics))

		history := HistoryEntry{
			TxHash:      ev.RawLog.TxHash.Hex(),
			Coll:        coll,
			Debt:        debt,
			TxType:      op,
			Timestamp:   time.Now().Format(time.RFC3339),
			BlockNumber: ev.RawLog.BlockNumber,
		}

		err := handlePositionUpdate(wallet, asset, op, coll, debt, history, ev.RawLog.BlockNumber)
		if err != nil {
			log.Printf("⚠️ Position update failed: %v", err)
		} else {
			log.Printf("✅ Position updated: %s | Coll: %.2f | Debt: %.2f | NLTV: %.2f%%", wallet, coll, debt, calculateNLTV(debt, coll))
		}
	}
}

// ------------------ Processor ------------------

func runProcessor(client *ethclient.Client) {
	troveManager := common.HexToAddress("0xE5d2644bE06c5b5d48b42AA7f9EAf27f0bC84265")
	borrowOps := common.HexToAddress("0x165fB19121ab4f74dC66C520866B9Ef4EB86AFf8")

	troveABI, err := abi.JSON(strings.NewReader(loadABI("abi/TroveManager.json")))
	if err != nil {
		log.Fatalf("❌ Failed to parse TroveManager ABI: %v", err)
	}

	borrowOpsABI, err := abi.JSON(strings.NewReader(loadABI("abi/BorrowOperations.json")))
	if err != nil {
		log.Fatalf("❌ Failed to parse BorrowerOperations ABI: %v", err)
	}

	startBlock := int64(3772000)
	batchSize := int64(500)

	for {
		latest, err := client.BlockNumber(context.Background())
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		for from := startBlock; from <= int64(latest); from += batchSize {
			to := from + batchSize - 1
			if to > int64(latest) {
				to = int64(latest)
			}

			query := ethereum.FilterQuery{
				FromBlock: big.NewInt(from),
				ToBlock:   big.NewInt(to),
				Addresses: []common.Address{troveManager, borrowOps},
			}

			logs, err := client.FilterLogs(context.Background(), query)
			if err != nil {
				continue
			}

			for _, vLog := range logs {
				ev := ContractEvent{
					Data:     map[string]interface{}{},
					RawLog:   vLog,
					Contract: vLog.Address.Hex(),
				}

				topic := vLog.Topics[0]
				if vLog.Address == troveManager {
					for name, event := range troveABI.Events {
						if event.ID == topic {
							ev.Name = name
							_ = troveABI.UnpackIntoMap(ev.Data, name, vLog.Data)
						}
					}
				} else if vLog.Address == borrowOps {
					for name, event := range borrowOpsABI.Events {
						if event.ID == topic {
							ev.Name = name
							_ = borrowOpsABI.UnpackIntoMap(ev.Data, name, vLog.Data)
						}
					}
				}
				handleEvent(ev, troveABI)
			}

			log.Printf("✅ Processed batch %d-%d (%d logs)", from, to, len(logs))
		}

		startBlock = int64(latest) + 1
		time.Sleep(10 * time.Second)
	}
}

// ------------------ Main ------------------

func main() {
	var err error
	mongoClient, err = mongo.Connect(context.TODO(), options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		log.Fatalf("❌ Failed to connect Mongo: %v", err)
	}

	positionColl = mongoClient.Database("test").Collection("PositionIndexer")
	historyColl = mongoClient.Database("test").Collection("PositionHistory")

	client, err := ethclient.Dial("https://rpc.ankr.com/botanix_testnet")
	if err != nil {
		log.Fatalf("❌ Failed to connect Ethereum: %v", err)
	}

	log.Println("🔹 Starting blockchain event processor...")
	runProcessor(client)
}

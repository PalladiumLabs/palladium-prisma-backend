package main

import (
	"context"
	"fmt"
	"log"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

func main() {
	client, err := ethclient.Dial("https://rpc.ankr.com/botanix_testnet")
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer client.Close()

	// Your contract addresses
	contracts := map[string]string{
		"TROVE_MANAGER":      "0xE5d2644bE06c5b5d48b42AA7f9EAf27f0bC84265",
		"BORROWER_OPERATIONS": "0x165fB19121ab4f74dC66C520866B9Ef4EB86AFf8",
		"SSS":                "0x67e0f0917eF5f5ae9940a68b49dD84D7214E036e",
		"PRICE_FEED":         "0x1f9866230b44d610d4fc66fdd742312D59c81355",
		"FACTORY":            "0x8CA929e38338675530f7Eb5f4020fF385Ed0B6Ad",
		"STABILITY_POOL":     "0xb9B14e83070232c3b3b2404dB7C21781bFf1537B",
	}

	fmt.Println("=== Contract Analysis ===")
	for name, addr := range contracts {
		address := common.HexToAddress(addr)
		code, err := client.CodeAt(context.Background(), address, nil)
		if err != nil {
			fmt.Printf("%s: Error getting code: %v\n", name, err)
			continue
		}
		fmt.Printf("%s (%s): %d bytes\n", name, addr, len(code))
		
		// Check if it's a proxy by looking for common proxy patterns
		if len(code) < 1000 && len(code) > 0 {
			fmt.Printf("  ^ Likely a PROXY contract (small bytecode)\n")
		}
	}

	// Check the active contracts we found
	activeAddresses := []string{
		"0x67e0f0917eF5f5ae9940a68b49dD84D7214E036e", // SSS
		"0xacC0a0cF13571d30B4b8637996F5D6D774d4fd62", // Unknown
		"0xba7E78011909e3501027FBc226a04DCC837a555D", // Unknown
	}

	fmt.Println("\n=== Active Contract Analysis ===")
	for _, addr := range activeAddresses {
		address := common.HexToAddress(addr)
		code, err := client.CodeAt(context.Background(), address, nil)
		if err != nil {
			fmt.Printf("%s: Error getting code: %v\n", addr, err)
			continue
		}
		fmt.Printf("%s: %d bytes\n", addr, len(code))

		// Get the actual events from this contract
		query := ethereum.FilterQuery{
			FromBlock: big.NewInt(3845413),
			ToBlock:   big.NewInt(3845500),
			Addresses: []common.Address{address},
		}

		logs, err := client.FilterLogs(context.Background(), query)
		if err != nil {
			fmt.Printf("  Error getting logs: %v\n", err)
			continue
		}

		fmt.Printf("  Events: %d\n", len(logs))
		for i, logEntry := range logs {
			if i < 3 { // Show first 3 events
				fmt.Printf("    Event %d: Block %d, TxHash: %s, Topic[0]: %s\n", 
					i, logEntry.BlockNumber, logEntry.TxHash.Hex(), logEntry.Topics[0].Hex())
			}
		}
	}

	// Let's also check for TroveUpdated events across ALL contracts
	fmt.Println("\n=== Searching for TroveUpdated Event Signature ===")
	
	// TroveUpdated event signature hash
	// This is keccak256("TroveUpdated(address,uint256,uint256,uint256,uint8)")
	troveUpdatedTopic := common.HexToHash("0x1aba88b6bea8b91b3b35c6d2e4ab17b2ee0c21b0a5f72d5e6ba40b1b11b5e17e") // This might not be exact
	
	fmt.Printf("Looking for TroveUpdated signature (this is a guess): %s\n", troveUpdatedTopic.Hex())
	
	// Try to find this event signature in recent blocks
	queryBroad := ethereum.FilterQuery{
		FromBlock: big.NewInt(3845413),
		ToBlock:   big.NewInt(3845600),
		Topics:    [][]common.Hash{}, // No topic filter - get all events
	}

	allLogs, err := client.FilterLogs(context.Background(), queryBroad)
	if err != nil {
		fmt.Printf("Error searching all events: %v\n", err)
	} else {
		fmt.Printf("Found %d total events in blocks 3845413-3845600\n", len(allLogs))
		
		// Group by topic[0] to see what event signatures are being used
		topicCount := make(map[string]int)
		topicToAddress := make(map[string][]string)
		
		for _, logEntry := range allLogs {
			if len(logEntry.Topics) > 0 {
				topic := logEntry.Topics[0].Hex()
				topicCount[topic]++
				
				// Track which addresses emit this event
				addr := logEntry.Address.Hex()
				found := false
				for _, existingAddr := range topicToAddress[topic] {
					if existingAddr == addr {
						found = true
						break
					}
				}
				if !found {
					topicToAddress[topic] = append(topicToAddress[topic], addr)
				}
			}
		}
		
		fmt.Println("\nEvent signatures found:")
		for topic, count := range topicCount {
			fmt.Printf("  %s: %d events from addresses: %v\n", topic, count, topicToAddress[topic])
		}
	}
}
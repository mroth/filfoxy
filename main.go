package main

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"math/big"
	"net/http"
	"os"
	"slices"
	"time"
)

var (
	ApiEndpoint = "https://filfox.info/api/v1"
)

var (
	attoFIL = big.NewInt(1e18)
)

type APITransactionsResponse struct {
	TotalCount int                 `json:"totalCount"`
	Transfers  []APITransferRecord `json:"transfers"`
	Types      []string            `json:"types"`
}

type APITransferRecord struct {
	Height    int    `json:"height"`
	Timestamp int    `json:"timestamp"`
	Message   string `json:"message"`
	From      string `json:"from"`
	To        string `json:"to"`
	Value     string `json:"value"` // in attoFIL as a string
	Type      string `json:"type"`  // [send, receive, miner-fee, burn-fee]
}

type Transfer struct {
	Height    int       `json:"height"`
	Timestamp time.Time `json:"timestamp"`
	MessageID string    `json:"message_id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Amount    *big.Int  `json:"amount"`
	MinerFee  *big.Int  `json:"miner_fee"`
	BurnFee   *big.Int  `json:"burn_fee"`
}

func (t Transfer) String() string {
	return fmt.Sprintf("[%s] %s: 📤 %.6s… -> %.6s…, 💸: %9.2f\t| ⛏️: %6v\t| 🔥: %6v",
		t.Timestamp, t.MessageID, t.From, t.To, attoFILToFIL(t.Amount), t.MinerFee, t.BurnFee)
}

func attoFILToFIL(atto *big.Int) float64 {
	if atto == nil {
		return 0
	}

	fil := new(big.Float).SetInt(atto)
	fil.Quo(fil, new(big.Float).SetInt(attoFIL))
	f, _ := fil.Float64()
	return f
}

func retrieveTransfers(wallet string) ([]APITransferRecord, error) {
	var allTransfers []APITransferRecord
	pageSize := 100
	page := 0

	for {
		req, err := http.NewRequest("GET", ApiEndpoint+"/address/"+wallet+"/transfers", nil)
		if err != nil {
			return nil, err
		}

		q := req.URL.Query()
		q.Add("pageSize", fmt.Sprintf("%d", pageSize))
		q.Add("page", fmt.Sprintf("%d", page))
		req.URL.RawQuery = q.Encode()

		slog.Debug("API call", "url", req.URL.String())
		client := http.DefaultClient
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("API call returned non-success code: %s", resp.Status)
		}

		decoder := json.NewDecoder(resp.Body)
		var apiResponse APITransactionsResponse
		err = decoder.Decode(&apiResponse)
		if err != nil {
			return nil, err
		}

		allTransfers = append(allTransfers, apiResponse.Transfers...)

		// Check if we have retrieved all records
		if len(allTransfers) >= apiResponse.TotalCount {
			break
		}

		page++
	}

	return allTransfers, nil
}

func mungeTransferRecords(records []APITransferRecord) ([]Transfer, error) {
	transferSet := make(map[string]Transfer, 0)

	for _, record := range records {
		// If first time we've seen this message, create a new Transfer
		transfer, found := transferSet[record.Message]
		if !found {
			transfer.Height = record.Height
			transfer.Timestamp = time.Unix(int64(record.Timestamp), 0).UTC()
			transfer.MessageID = record.Message
			transfer.From = record.From
			transfer.To = record.To
			transferSet[record.Message] = transfer
		}

		// Parse the amount and assign it to the Transfer
		value, ok := new(big.Int).SetString(record.Value, 10)
		if !ok {
			return nil, fmt.Errorf("Failed to parse amount %s", record.Value)
		}
		value = value.Abs(value)
		switch record.Type {
		case "send", "receive":
			transfer.Amount = value
			transferSet[record.Message] = transfer
		case "burn-fee":
			transfer.BurnFee = value
			transferSet[record.Message] = transfer
		case "miner-fee":
			transfer.MinerFee = value
			transferSet[record.Message] = transfer
		default:
			return nil, fmt.Errorf("Unknown transfer type: %s", record.Type)
		}
	}

	// verification -- make sure all transfers have amount field set
	for _, transfer := range transferSet {
		if transfer.Amount == nil {
			return nil, fmt.Errorf("Transfer %s is missing amount fields", transfer.MessageID)
		}
	}

	xfers := slices.Collect(maps.Values(transferSet))
	slices.SortFunc(xfers, func(a, b Transfer) int {
		return b.Timestamp.Compare(a.Timestamp)
	})
	return xfers, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <wallet>", os.Args[0])
		os.Exit(1)
	}
	wallet := os.Args[1]

	slog.SetLogLoggerLevel(slog.LevelDebug)

	log.Printf("Retrieving transactions for wallet %s", wallet)
	xferRecs, err := retrieveTransfers(wallet)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Received %d transactions, munging...", len(xferRecs))
	xfers, err := mungeTransferRecords(xferRecs)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Munged into %d transfers", len(xfers))

	for _, xfer := range xfers {
		fmt.Println(xfer)
	}
}

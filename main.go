package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
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

func attoFILToFIL(atto *big.Int) *big.Float {
	if atto == nil {
		return big.NewFloat(0)
	}

	fil := new(big.Float).SetInt(atto)
	fil.Quo(fil, new(big.Float).SetInt(attoFIL))
	return fil
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

// Write a Ledger style CSV file
func writeLedgerCSV(w io.Writer, xfers []Transfer) error {
	writer := csv.NewWriter(w)
	defer writer.Flush()

	// Write CSV header
	headers := []string{
		"Operation Date",      // Field 1: "Operation Date", as 2024-09-12T16:19:30.000Z format
		"Status",              // Field 2: "Status" --> hard code to "Confirmed" (for now, can check height later, but not necessary for my use case)
		"Currency Ticker",     // Field 3: "Currency Ticker" --> hard code to "FIL"
		"Operation Type",      // Field 4: "Operation Type" --> ["IN" or "OUT"] based on transfer direction
		"Operation Amount",    // Field 5: "Operation Amount" --> FIL amount transferred, absolute value
		"Operation Fees",      // Field 6: "Operation Fees" --> miner fee + burn fees, if any
		"Operation Hash",      // Field 7: "Opearation Hash" --> the message ID
		"Account Name",        // Field 8: "Account Name" --> hard code to "Filfox API"
		"Account xpub",        // Field 9: "Account xpub" --> sender or receiver address
		"Countervalue Ticker", // Field 10: "Countervalue Ticker" --> hard code to "USD"
		// Field 11: "Countervalue at Operation Date" -> Omitted, we want to import cost basis from another source rather than rely on Filfox's spot exchange rate
		// Field 12: "Countervalue at CSV Export" -> Omitted, not valuable for this use case
	}
	if err := writer.Write(headers); err != nil {
		return err
	}

	// Write CSV records
	for _, xfer := range xfers {
		// Field 1: Operation Date
		const iso8601WithMillis = "2006-01-02T15:04:05.000Z"
		operationDate := xfer.Timestamp.Format(iso8601WithMillis)

		// Field 2: Status
		status := "Confirmed"

		// Field 3: Currency Type
		currencyType := "FIL"

		// Field 4: Operation Type and Field 9: Account xpub
		var operationType, accountXpub string
		if xfer.Amount.Cmp(big.NewInt(0)) > 0 {
			operationType = "IN"
			accountXpub = xfer.To
		} else {
			operationType = "OUT"
			accountXpub = xfer.From
		}
		// Field 5: Operation Amount
		// Needs to be converted to abs value, as Filfox API returns negative values for OUT transactions
		// On OUT transactions, Ledger add the totalFee to the amount, so we need to calculate the totalFee first
		totalFee := new(big.Int)
		if xfer.MinerFee != nil {
			totalFee.Add(totalFee, xfer.MinerFee)
		}
		if xfer.BurnFee != nil {
			totalFee.Add(totalFee, xfer.BurnFee)
		}

		var amount *big.Int
		if operationType == "OUT" {
			amount = new(big.Int).Add(new(big.Int).Abs(xfer.Amount), new(big.Int).Abs(totalFee))
		} else {
			amount = new(big.Int).Abs(xfer.Amount)
		}
		// For formatting float64 to here, only use enough precision as necessary, but allow up to 18 digits of precision
		_amount := attoFILToFIL(amount)
		operationAmount := _amount.Text('f', -1)

		// Field 6: Operation Fee
		// Calculated in previous field
		_fee := attoFILToFIL(new(big.Int).Abs(totalFee))
		operationFee := _fee.Text('f', -1)

		// Field 7: Operation Hash
		operationHash := xfer.MessageID

		// Field 8: Account Name
		accountName := "Filfox API"

		// Field 9: Account xpub
		if xfer.Amount.Cmp(big.NewInt(0)) > 0 {
			accountXpub = xfer.To
		} else {
			accountXpub = xfer.From
		}

		// Field 10: Countervalue Ticker
		counterValueTicker := "USD"

		record := []string{
			operationDate,
			status,
			currencyType,
			operationType,
			operationAmount,
			operationFee,
			operationHash,
			accountName,
			accountXpub,
			counterValueTicker,
		}

		if err := writer.Write(record); err != nil {
			return err
		}
	}

	return nil
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

	outputFileName := fmt.Sprintf("%s.csv", wallet[:9])
	file, err := os.Create(outputFileName)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	err = writeLedgerCSV(file, xfers)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Transfers written to %s", outputFileName)
}

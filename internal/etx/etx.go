package etx

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

var (
	DB      *gorm.DB
	Clients = make(map[string]*ethclient.Client)
)

// Transaction contains the data for a single transaction
// on the Ethereum Blockchain
type Transaction struct {
	gorm.Model
	ID         string      `json:"txid"`
	Blockchain string      `json:"blockchain"`
	Metadata   MetadataMap `json:"metadata"`
	Monitoring bool        `json:"monitoring"`
	Pending    bool        `json:"pending"`
	Checks     int         `json:"checks"`
	Success    bool        `json:"success"`
	Reviewed   bool        `json:"reviewed"`
	Error      string      `json:"error"`
}

type MetadataMap map[string]string

func (m MetadataMap) Value() (driver.Value, error) {
	return json.Marshal(m)
}

func (m *MetadataMap) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("[]byte assertion failed")
	}
	return json.Unmarshal(b, m)
}

func GetBlockchainClient(name string) (*ethclient.Client, error) {
	var c *ethclient.Client
	if c, ok := Clients[name]; ok {
		return c, nil
	}
	return c, errors.New("blockchain client not found")
}

// ChecksThreshold will automatically mark a transaction as failed if it
// has been checked N number of times and still has not definitively succeeded or failed
func (t *Transaction) ChecksThreshold() {
	log.WithFields(log.Fields{
		"action": "transaction.ChecksThreshold",
		"txid":   t.ID,
	}).Printf("checks=%d", t.Checks)
	sc, serr := strconv.Atoi(os.Getenv("CHECKS_THRESHOLD"))
	if serr != nil {
		log.Printf("error %v", serr)
		return
	}
	if t.Checks > sc {
		t.Error = "exceeded checks threshold"
		t.Monitoring = false
		t.Pending = false
		t.Success = false
	}
}

// Save saves a transaction in the database. If the number of checks exceeds the ChecksThreshold
// it will mark the transaction as failed
func (t *Transaction) Save() error {
	log.WithFields(log.Fields{
		"action": "transaction.Save",
		"txid":   t.ID,
	}).Printf("%+v", t)
	t.ChecksThreshold()
	ut := map[string]interface{}{
		"success":    t.Success,
		"pending":    t.Pending,
		"error":      t.Error,
		"monitoring": t.Monitoring,
		"checks":     t.Checks,
	}
	DB.Find(&Transaction{ID: t.ID}).Updates(ut)
	return nil
}

// CheckSuccess checks whether a transaction is pending, errored, or successful
// and logs the state in the database.
func (t *Transaction) CheckSuccess(ctx context.Context) error {
	log.WithFields(log.Fields{
		"action": "transaction.CheckSuccess",
		"txid":   t.ID,
		"checks": t.Checks,
	}).Print("")
	t.Checks++
	txHash := common.HexToHash(t.ID)
	c, cerr := GetBlockchainClient(t.Blockchain)
	if cerr != nil {
		return cerr
	}
	tx, isPending, err := c.TransactionByHash(ctx, txHash)
	if err != nil {
		log.Println(err)
		t.Pending = false
		t.Monitoring = false
		t.Error = err.Error()
		t.Save()
		return err
	}
	if isPending {
		t.Pending = true
		t.Monitoring = true
	} else {
		t.Pending = false
		t.Monitoring = false
		r, err := c.TransactionReceipt(ctx, tx.Hash())
		if err != nil {
			log.Println(err)
			t.Error = err.Error()
			t.Save()
			return err
		}
		if r.Status > 0 {
			t.Success = true
		} else {
			// Todo capture r.Logs data
			t.Success = false
			t.Error = "failure"
		}
	}
	t.Save()
	return nil
}

// HttpJSON marshals a transaction into a JSON response and sends it through the
// provided http.ResponseWriter
func (t *Transaction) HttpJSON(w http.ResponseWriter) {
	log.WithFields(log.Fields{
		"action": "transaction.HttpJSON",
		"txid":   t.ID,
	}).Print("Create response JSON")
	jd, jerr := json.Marshal(t)
	if jerr != nil {
		log.Println(jerr)
		http.Error(w, jerr.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprint(w, string(jd))
}

// New creates a new record of a transaction in the monitor system
func (t *Transaction) New() error {
	log.WithFields(log.Fields{
		"action": "transaction.New",
		"txid":   t.ID,
	}).Print("Create new transaction")
	t.Monitoring = true
	tx := DB.Create(t)
	if tx.Error != nil {
		return tx.Error
	}
	return nil
}

// SetSuccess sets the success field on a transaction
func (t *Transaction) SetSuccess() error {
	log.WithFields(log.Fields{
		"action": "transaction.SetSuccess",
		"txid":   t.ID,
	}).Printf("Set Success: %v", t.Success)
	DB.Find(&Transaction{ID: t.ID}).Update("success", t.Success)
	return nil
}

// SetReviewed sets the reviewed field on a transaction
func (t *Transaction) SetReviewed() error {
	log.WithFields(log.Fields{
		"action": "transaction.SetReviewed",
		"txid":   t.ID,
	}).Printf("Set reviewed: %v", t.Reviewed)
	DB.Find(&Transaction{ID: t.ID}).Update("reviewed", t.Reviewed)
	return nil
}

// MonitoredTransactions retrieves all Monitored (and unreviewed)
// transactions from the database
func MonitoredTransactions() ([]Transaction, error) {
	log.WithFields(log.Fields{
		"action": "MonitoredTransactions",
	}).Printf("get")
	var txs []Transaction
	DB.Find(
		&txs,
		&Transaction{
			Monitoring: true,
			Reviewed:   false,
		},
	)
	return txs, nil
}

// monitorWorker concurrently checks transactions as they are received
func monitorWorker(ctx context.Context, tin <-chan *Transaction, tout chan<- *Transaction) {
	for t := range tin {
		t.CheckSuccess(ctx)
		tout <- t
	}
}

// CheckMonitoredTransactions loops through all Monitored Transactions
// and checks their current status on the blockchain
func CheckMonitoredTransactions(ctx context.Context) error {
	log.WithFields(log.Fields{
		"action": "CheckMonitoredTransactions",
	}).Printf("run")
	txs, err := MonitoredTransactions()
	if err != nil {
		return err
	}
	tin := make(chan *Transaction, len(txs))
	tout := make(chan *Transaction, len(txs))
	for w := 0; w < 10; w++ {
		go monitorWorker(ctx, tin, tout)
	}
	for _, t := range txs {
		tin <- &t
	}
	close(tin)
	for i := 0; i < len(txs); i++ {
		<-tout
	}
	return nil
}

func Ping(db *gorm.DB) error {
	d, derr := db.DB()
	if derr != nil {
		return derr
	}
	return d.Ping()
}

func Healthcheck() error {
	if err := Ping(DB); err != nil {
		return err
	}
	return nil
}

func Healthchecker() error {
	for {
		if err := Healthcheck(); err != nil {
			log.Fatal(err)
			return err
		}
		time.Sleep(time.Second * 10)
	}
}

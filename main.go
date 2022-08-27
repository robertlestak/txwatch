package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/robertlestak/txwatch/internal/etx"
	log "github.com/sirupsen/logrus"

	"github.com/gorilla/mux"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// HandleNewTransaction is an HTTP handler to receive a new transaction
// event and add this transaction to the monitor
func HandleNewTransaction(w http.ResponseWriter, r *http.Request) {
	log.WithFields(log.Fields{
		"action": "HandleNewTransaction",
	})
	log.Println("New Transaction Request")
	defer r.Body.Close()
	bd, berr := ioutil.ReadAll(r.Body)
	if berr != nil {
		log.Println(berr)
		http.Error(w, berr.Error(), http.StatusBadRequest)
		return
	}
	t := &etx.Transaction{}
	jerr := json.Unmarshal(bd, &t)
	if jerr != nil {
		log.Println(jerr)
		http.Error(w, jerr.Error(), http.StatusBadRequest)
		return
	}
	log.WithFields(log.Fields{
		"action": "HandleNewTransaction",
	}).Printf("txid=%s blockchainID=%s", t.ID, t.Blockchain)
	terr := t.New()
	if terr != nil {
		log.Println(terr)
		http.Error(w, terr.Error(), http.StatusBadRequest)
		return
	}
}

// HandleSetReviewed is an HTTP handler to receive a request
// to set the "reviewed" state of a transaction by txid
func HandleSetReviewed(w http.ResponseWriter, r *http.Request) {
	log.WithFields(log.Fields{
		"action": "HandleSetReviewed",
	})
	log.Println("HandleSetReviewed")
	vars := mux.Vars(r)
	defer r.Body.Close()
	bd, berr := ioutil.ReadAll(r.Body)
	if berr != nil {
		log.Println(berr)
		http.Error(w, berr.Error(), http.StatusBadRequest)
		return
	}
	t := &etx.Transaction{}
	jerr := json.Unmarshal(bd, &t)
	if jerr != nil {
		log.Println(jerr)
		http.Error(w, jerr.Error(), http.StatusBadRequest)
		return
	}
	t.ID = vars["txid"]
	log.Printf("txid=%s", t.ID)
	terr := t.SetReviewed()
	if terr != nil {
		log.Println(terr)
		http.Error(w, terr.Error(), http.StatusBadRequest)
		return
	}
	etx.DB.Find(t, &etx.Transaction{ID: t.ID})
	t.HttpJSON(w)
}

func Paginate(r *http.Request) func(db *gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		page, _ := strconv.Atoi(r.FormValue("page"))
		if page == 0 {
			page = 1
		}

		pageSize, _ := strconv.Atoi(r.FormValue("pageSize"))
		switch {
		case pageSize > 100:
			pageSize = 100
		case pageSize <= 0:
			pageSize = 10
		}

		offset := (page - 1) * pageSize
		return db.Offset(offset).Limit(pageSize)
	}
}

// HandleGetTransactions is an HTTP handler to retrieve transaction
// details from the database
func HandleGetTransactions(w http.ResponseWriter, r *http.Request) {
	log.WithFields(log.Fields{
		"action": "HandleGetTransaction",
	}).Println("Get Transaction Request")
	defer r.Body.Close()
	t := &etx.Transaction{}
	bd, berr := ioutil.ReadAll(r.Body)
	if berr != nil {
		log.Printf("error %v", berr)
		http.Error(w, berr.Error(), http.StatusBadRequest)
		return
	}
	jerr := json.Unmarshal(bd, &t)
	if jerr != nil {
		log.Printf("error %v", jerr)
		http.Error(w, jerr.Error(), http.StatusBadRequest)
		return
	}
	var ot []etx.Transaction
	etx.DB.Scopes(Paginate(r)).Find(&ot, t)
	jd, jerr := json.Marshal(ot)
	if jerr != nil {
		log.Printf("error %v", jerr)
		http.Error(w, jerr.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprint(w, string(jd))
}

func init() {
	var err error
	log.Printf("connecting to database")
	dsn := fmt.Sprintf("host=%s port=%s user=%s dbname=%s password=%s sslmode=disable",
		os.Getenv("DB_HOST"),
		os.Getenv("DB_PORT"),
		os.Getenv("DB_USER"),
		os.Getenv("DB_NAME"),
		os.Getenv("DB_PASSWORD"),
	)
	etx.DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	etx.DB.AutoMigrate(&etx.Transaction{})
	log.Printf("connecting to ethereum: %s\n", os.Getenv("ETH_ENDPOINT"))
	for _, e := range strings.Split(os.Getenv("ETH_ENDPOINTS"), ",") {
		e = strings.TrimSpace(e)
		ss := strings.Split(e, "=")
		if len(ss) != 2 {
			log.Fatal("ETH_ENDPOINTS must be in the form of '<name>=<endpoint>'")
		}
		name := strings.TrimSpace(ss[0])
		endpoint := strings.TrimSpace(ss[1])
		log.Printf("connecting to ethereum: client=%s host=%s", name, endpoint)
		var c *ethclient.Client
		c, err = ethclient.Dial(endpoint)
		if err != nil {
			log.Fatal("ethclient error", err)
		}
		etx.Clients[name] = c
	}
	go etx.Healthchecker()
}

func HandleHealthCheck(w http.ResponseWriter, r *http.Request) {
	for _, c := range etx.Clients {
		_, err := c.ChainID(context.Background())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	fmt.Fprint(w, "healthy")
}

func api() {
	r := mux.NewRouter()
	r.HandleFunc("/transaction", HandleNewTransaction).Methods("POST")
	r.HandleFunc("/transaction/{txid}/reviewed", HandleSetReviewed).Methods("POST")
	r.HandleFunc("/transactions", HandleGetTransactions).Methods("POST")
	r.HandleFunc("/status/healthz", HandleHealthCheck).Methods("GET")
	log.Printf("Listening on :%s\n", os.Getenv("PORT"))
	http.ListenAndServe(":"+os.Getenv("PORT"), r)
}

func worker() {
	log.WithFields(log.Fields{
		"action": "worker",
	}).Println("run")
	ctx := context.Background()
	ct, cerr := strconv.Atoi(os.Getenv("CHECKS_TIMER"))
	if cerr != nil {
		log.Fatal(cerr)
	}
	for {
		etx.CheckMonitoredTransactions(ctx)
		time.Sleep(time.Second * time.Duration(ct))
	}
}

func main() {
	go worker()
	api()
}

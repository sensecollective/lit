package powless

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/mit-dci/lit/lnutil"
)

// powless is a couple steps below uspv in that it doesn't check
// proof of work.  It just asks some web API thing about if it has
// received money or not.

/*
implement this:

ChainHook interface

	Start(height int32, host, path string, params *chaincfg.Params) (
		chan lnutil.TxAndHeight, chan int32, error)

	RegisterAddress(address [20]byte) error
	RegisterOutPoint(wire.OutPoint) error

	PushTx(tx *wire.MsgTx) error

	RawBlocks() chan *wire.MsgBlock

*/

type APILink struct {
	apiCon net.Conn

	// TrackingAdrs and OPs are slices of addresses and outpoints to watch for.
	// Using struct{} saves a byte of RAM but is ugly so I'll use bool.
	TrackingAdrs    map[[20]byte]bool
	TrackingAdrsMtx sync.Mutex

	TrackingOPs    map[wire.OutPoint]bool
	TrackingOPsMtx sync.Mutex

	TxUpToWallit chan lnutil.TxAndHeight

	CurrentHeightChan chan int32

	// time based polling
	dirtybool bool

	p *chaincfg.Params
}

func (a *APILink) Start(
	startHeight int32, host, path string, params *chaincfg.Params) (
	chan lnutil.TxAndHeight, chan int32, error) {

	// later, use params to detect which api to connect to
	a.p = params

	a.TrackingAdrs = make(map[[20]byte]bool)
	a.TrackingOPs = make(map[wire.OutPoint]bool)

	a.TxUpToWallit = make(chan lnutil.TxAndHeight, 1)
	a.CurrentHeightChan = make(chan int32, 1)

	go a.ClockLoop()

	return a.TxUpToWallit, a.CurrentHeightChan, nil
}

func (a *APILink) ClockLoop() {

	for {
		if a.dirtybool {
			a.dirtybool = false
			err := a.GetAdrTxos()
			if err != nil {
				fmt.Printf(err.Error())
			}
		} else {
			fmt.Printf("clean, sleep 5 sec\n")
			time.Sleep(time.Second * 5)
		}
	}

	return
}

func (a *APILink) RegisterAddress(adr160 [20]byte) error {
	a.TrackingAdrsMtx.Lock()
	a.TrackingAdrs[adr160] = true
	a.TrackingAdrsMtx.Unlock()
	a.dirtybool = true
	return nil
}

func (a *APILink) RegisterOutPoint(op wire.OutPoint) error {
	a.TrackingOPsMtx.Lock()
	a.TrackingOPs[op] = true
	a.TrackingOPsMtx.Unlock()
	a.dirtybool = true
	return nil
}

// ARGHGH all fields have to be exported (caps) or the json unmarshaller won't
// populate them !
type AdrResponse struct {
	Success bool
	//	Paging  interface{}
	Unspent []jsutxo
}

type TxResponse struct {
	Success     bool
	Transaction []TxJson
}

type TxJson struct {
	Block int32
	Txid  string
}

type TxHexResponse struct {
	Success bool
	Hex     []TxHexString
}

type TxHexString struct {
	Txid string
	Hex  string
}

type jsutxo struct {
	Value_int int64
	Txid      string
	N         uint32
	Addresses []string // why more than 1 ..?
}

func (a *APILink) GetAdrTxos() error {
	apitxourl := "https://testnet-api.smartbit.com.au/v1/blockchain/address/"
	// make a comma-separated list of base58 addresses
	var adrlist string

	a.TrackingAdrsMtx.Lock()
	for adr160, _ := range a.TrackingAdrs {
		adr58, err := btcutil.NewAddressPubKeyHash(adr160[:], a.p)
		if err != nil {
			return err
		}
		adrlist += adr58.String()
		adrlist += ","
	}

	// chop off last comma
	adrlist = adrlist[:len(adrlist)-1] + "/unspent"

	response, err := http.Get(apitxourl + adrlist)
	if err != nil {
		return err
	}

	ar := new(AdrResponse)

	err = json.NewDecoder(response.Body).Decode(ar)
	if err != nil {
		return err
	}

	if !ar.Success {
		return fmt.Errorf("ar success = false...")
	}

	var txidlist string

	// go through all unspent txos.  All we want is the txids, to request the
	// full txs.
	for i, txo := range ar.Unspent {
		txidlist += txo.Txid + ","
	}
	txidlist = txidlist[:len(txidlist)-1] + "/hex"
	// now request all those txids
	// need to request twice! To find height.  Blah.

	apitxurl := "https://testnet-api.smartbit.com.au/v1/blockchain/tx/"

	response, err = http.Get(apitxurl + txidlist)
	if err != nil {
		return err
	}

	tr := new(TxResponse)

	err = json.NewDecoder(response.Body).Decode(tr)
	if err != nil {
		return err
	}

	if !tr.Success {
		return fmt.Errorf("tr success = false...")
	}

	chainhash.NewHashFromStr()
	for _, txjson := range tr.Hex {
		buf, err := hex.DecodeString(txjson.Hex)
		if err != nil {
			return err
		}
		buf := bytes.NewBuffer(buf)
		tx := wire.NewMsgTx()
		err = tx.Deserialize(buf)
		if err != nil {
			return err
		}
		//		a.TxUpToWallit
	}

	return nil
}

func (a *APILink) PushTx(tx *wire.MsgTx) error {
	if tx == nil {
		return fmt.Errorf("tx is nil")
	}
	var b bytes.Buffer

	err := tx.Serialize(&b)
	if err != nil {
		return err
	}

	// turn into hex
	txHexString := fmt.Sprintf("{\"hex\": \"%x\"}", b.Bytes())

	fmt.Printf("tx hex string is %s\n", txHexString)

	apiurl := "https://testnet-api.smartbit.com.au/v1/blockchain/pushtx"
	response, err := http.Post(
		apiurl, "application/json", bytes.NewBuffer([]byte(txHexString)))
	fmt.Printf("respo	nse: %s", response.Status)
	_, err = io.Copy(os.Stdout, response.Body)

	return err
}

func (a *APILink) RawBlocks() chan *wire.MsgBlock {
	// dummy channel for now
	return make(chan *wire.MsgBlock, 1)
}
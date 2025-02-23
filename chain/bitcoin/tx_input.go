package bitcoin

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"

	xc "github.com/jumpcrypto/crosschain"
	log "github.com/sirupsen/logrus"
)

// TxInput for Bitcoin
type TxInput struct {
	xc.TxInputEnvelope
	UnspentOutputs  []Output            `json:"unspent_outputs"`
	Inputs          []Input             `json:"input"`
	FromPublicKey   []byte              `json:"from_public_key"`
	GasPricePerByte xc.AmountBlockchain `json:"gas_price_per_byte"`
}

var _ xc.TxInputWithPublicKey = &TxInput{}

// NewTxInput returns a new Bitcoin TxInput
func NewTxInput() *TxInput {
	return &TxInput{
		TxInputEnvelope: *xc.NewTxInputEnvelope(xc.DriverBitcoin),
	}
}

func (txInput *TxInput) GetGetPricePerByte() xc.AmountBlockchain {
	return txInput.GasPricePerByte
}
func (txInput *TxInput) SetPublicKey(publicKeyBytes xc.PublicKey) error {
	txInput.FromPublicKey = publicKeyBytes
	return nil
}

func (txInput *TxInput) SetPublicKeyFromStr(publicKeyStr string) error {
	publicKeyBytes, err := base64.StdEncoding.DecodeString(publicKeyStr)
	if err != nil {
		return fmt.Errorf("invalid public key %v: %v", publicKeyStr, err)
	}
	err = txInput.SetPublicKey(publicKeyBytes)

	return err
}

// 1. sort unspentOutputs from lowest to highest
// 2. grab the minimum amount of UTXO needed to satify amount
// 3. tack on the smallest utxo's until `minUtxo` is reached.
// This ensures a small number of UTXO are used for transaction while also consolidating some
// smaller utxo into the transaction.
// Returns the total balance of the min utxo set.  txInput.inputs are updated to the new set.
func (txInput *TxInput) allocateMinUtxoSet(targetAmount xc.AmountBlockchain, minUtxo int) *xc.AmountBlockchain {
	balance := xc.NewAmountBlockchainFromUint64(0)

	// 1. sort from lowest to higher
	if len(txInput.UnspentOutputs) > 1 {
		sort.Slice(txInput.UnspentOutputs, func(i, j int) bool {
			return txInput.UnspentOutputs[i].Value.Cmp(&txInput.UnspentOutputs[j].Value) <= 0
		})
	}

	inputs := []Input{}
	lenUTXOIndex := len(txInput.UnspentOutputs) - 1
	for balance.Cmp(&targetAmount) < 0 && lenUTXOIndex >= 0 {
		o := txInput.UnspentOutputs[lenUTXOIndex]
		log.Infof("unspent output h2l: %s (%s)", hex.EncodeToString(o.PubKeyScript), o.Value.String())
		balance = balance.Add(&o.Value)

		inputs = append(inputs, Input{
			Output: o,
		})
		lenUTXOIndex--
	}

	// add the smallest utxo until we reach `minUtxo` inputs
	// lenUTXOIndex wasn't used, so i can grow up to lenUTXOIndex (included)
	i := 0
	for len(inputs) < minUtxo && i < lenUTXOIndex {
		o := txInput.UnspentOutputs[i]
		log.Infof("unspent output l2h: %s (%s)", hex.EncodeToString(o.PubKeyScript), o.Value.String())
		balance = balance.Add(&o.Value)
		inputs = append(inputs, Input{
			Output: o,
		})
		i++
	}
	txInput.Inputs = inputs
	return &balance
}

package cosmos

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	xc "github.com/jumpcrypto/crosschain"

	// injectivecryptocodec "github.com/InjectiveLabs/sdk-go/chain/crypto/codec"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

// TxInput for Cosmos
type TxInput struct {
	xc.TxInputEnvelope
	AccountNumber uint64
	Sequence      uint64
	GasLimit      uint64
	GasPrice      float64
	Memo          string
	FromPublicKey []byte
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
	return txInput.SetPublicKey(publicKeyBytes)
}

// NewTxInput returns a new Cosmos TxInput
func NewTxInput() *TxInput {
	return &TxInput{
		TxInputEnvelope: *xc.NewTxInputEnvelope(xc.DriverCosmos),
	}
}

// Client for Cosmos
type Client struct {
	Asset           xc.ITask
	Ctx             client.Context
	Prefix          string
	EstimateGasFunc xc.EstimateGasFunc
}

var _ xc.FullClientWithGas = &Client{}

// NewClient returns a new Client
func NewClient(cfgI xc.ITask) (*Client, error) {
	asset := cfgI
	cfg := cfgI.GetNativeAsset()
	host := cfg.URL
	httpClient, err := rpchttp.NewWithClient(
		host,
		"websocket",
		&http.Client{
			// Timeout: opts.Timeout,

			// We override the transport layer with a custom implementation as
			// there is an issue with the Cosmos SDK that causes it to
			// incorrectly parse URLs.
			Transport: newTransport(host, &http.Transport{}),
		})
	if err != nil {
		panic(err)
	}

	cosmosCfg := MakeCosmosConfig()
	cliCtx := client.Context{}.
		WithClient(httpClient).
		WithCodec(cosmosCfg.Marshaler).
		WithTxConfig(cosmosCfg.TxConfig).
		WithLegacyAmino(cosmosCfg.Amino).
		WithInterfaceRegistry(cosmosCfg.InterfaceRegistry).
		WithBroadcastMode("sync").
		WithChainID(string(cfg.ChainIDStr))

	return &Client{
		Asset:           asset,
		Ctx:             cliCtx,
		Prefix:          cfg.ChainPrefix,
		EstimateGasFunc: nil,
	}, nil
}

type transport struct {
	remote string
	proxy  http.RoundTripper
}

func newTransport(remote string, proxy http.RoundTripper) *transport {
	return &transport{
		remote: remote,
		proxy:  proxy,
	}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.remote)
	if err != nil {
		return nil, err
	}
	req.URL = u
	req.Host = u.Host

	// Proxy request.
	return t.proxy.RoundTrip(req)
}

// FetchTxInput returns tx input for a Cosmos tx
func (client *Client) FetchTxInput(ctx context.Context, from xc.Address, _ xc.Address) (xc.TxInput, error) {
	txInput := NewTxInput()

	account, err := client.GetAccount(ctx, from)
	if err != nil || account == nil {
		return txInput, fmt.Errorf("failed to get account data for %v: %v", from, err)
	}
	txInput.AccountNumber = account.GetAccountNumber()
	txInput.Sequence = account.GetSequence()

	gasPrice, err := client.EstimateGas(ctx)
	if err != nil {
		return txInput, fmt.Errorf("failed to estimate gas: %v", err)
	}
	txInput.GasPrice = gasPrice.UnmaskFloat64()

	return txInput, nil
}

// SubmitTx submits a Cosmos tx
func (client *Client) SubmitTx(ctx context.Context, txInput xc.Tx) error {
	tx := txInput.(*Tx)
	txBytes, _ := tx.Serialize()
	txID := tx.Hash()

	res, err := client.Ctx.BroadcastTx(txBytes)
	if err != nil {
		return fmt.Errorf("failed to broadcast tx %v: %v", txID, err)
	}

	if res.Code != 0 {
		return fmt.Errorf("tx %v failed code: %v, log: %v", txID, res.Code, res.RawLog)
	}

	return nil
}

// FetchTxInfo returns tx info for a Cosmos tx
func (client *Client) FetchTxInfo(ctx context.Context, txHash xc.TxHash) (xc.TxInfo, error) {
	result := xc.TxInfo{
		Fee:           xc.AmountBlockchain{},
		BlockIndex:    0,
		BlockTime:     0,
		Confirmations: 0,
	}

	hash, err := hex.DecodeString(string(txHash))
	if err != nil {
		return result, err
	}

	resultRaw, err := client.Ctx.Client.Tx(ctx, hash, false)
	if err != nil {
		return result, err
	}
	// log.Println(resultRaw)

	blockResultRaw, err := client.Ctx.Client.Block(ctx, &resultRaw.Height)
	if err != nil {
		return result, err
	}

	abciInfo, err := client.Ctx.Client.ABCIInfo(ctx)
	if err != nil {
		return result, err
	}

	decoder := client.Ctx.TxConfig.TxDecoder()
	decodedTx, err := decoder(resultRaw.Tx)
	if err != nil {
		return result, err
	}

	tx := &Tx{
		CosmosTx:        decodedTx,
		CosmosTxEncoder: client.Ctx.TxConfig.TxEncoder(),
	}

	result.TxID = string(txHash)
	result.ExplorerURL = client.Asset.GetNativeAsset().ExplorerURL + "/tx/" + result.TxID
	tx.ParseTransfer()

	// parse tx info - this should happen after ATA is set
	// (in most cases it works also in case or error)
	result.From = tx.From()
	result.To = tx.To()
	result.ContractAddress = tx.ContractAddress()
	result.Amount = tx.Amount()
	result.Fee = tx.Fee()
	result.Sources = tx.Sources()
	result.Destinations = tx.Destinations()

	result.BlockIndex = resultRaw.Height
	result.BlockTime = blockResultRaw.Block.Header.Time.Unix()
	result.Confirmations = abciInfo.Response.LastBlockHeight - result.BlockIndex

	if resultRaw.TxResult.Code != 0 {
		result.Status = xc.TxStatusFailure
	}

	return result, nil
}

// GetAccount returns a Cosmos account
// Equivalent to client.Ctx.AccountRetriever.GetAccount(), but doesn't rely GetConfig()
func (client *Client) GetAccount(ctx context.Context, address xc.Address) (client.Account, error) {
	_, err := types.GetFromBech32(string(address), client.Prefix)
	if err != nil {
		return nil, fmt.Errorf("bad address: '%v': %v", address, err)
	}

	res, err := authtypes.NewQueryClient(client.Ctx).Account(ctx, &authtypes.QueryAccountRequest{Address: string(address)})
	if err != nil {
		return nil, err
	}

	var acc authtypes.AccountI
	if err := client.Ctx.InterfaceRegistry.UnpackAny(res.Account, &acc); err != nil {
		return nil, err
	}

	return acc, nil
}

func (client *Client) estimateGasFcd(ctx context.Context) (xc.AmountBlockchain, error) {
	zero := xc.NewAmountBlockchainFromUint64(0)
	asset := client.Asset
	fdcURL := asset.GetNativeAsset().FcdURL
	resp, err := http.Get(fdcURL + "/v1/txs/gas_prices")
	if err != nil {
		return zero, err
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, err
	}

	prices := make(map[string]string)
	err = json.Unmarshal(body, &prices)
	if err != nil {
		return zero, err
	}

	denom := asset.GetNativeAsset().ChainCoin
	priceStr, ok := prices[denom]
	if !ok {
		return zero, fmt.Errorf("could not find %s in /gas_prices", denom)
	}
	gasPrice, err := strconv.ParseFloat(priceStr, 64)
	if err != nil {
		return zero, err
	}

	multiplier := 1.0
	if asset.GetNativeAsset().ChainGasMultiplier > 0 {
		multiplier = asset.GetNativeAsset().ChainGasMultiplier
	}
	return xc.NewAmountBlockchainToMaskFloat64(gasPrice * multiplier), nil
}

// EstimateGas estimates gas price for a Cosmos chain
func (client *Client) EstimateGas(ctx context.Context) (xc.AmountBlockchain, error) {
	// invoke EstimateGasFunc callback, if registered
	if client.EstimateGasFunc != nil {
		nativeAsset := client.Asset.GetNativeAsset().NativeAsset
		res, err := client.EstimateGasFunc(nativeAsset)
		if err != nil {
			// continue with default implementation as fallback
		} else {
			return res, err
		}
	}

	zero := xc.NewAmountBlockchainFromUint64(0)
	if client.Asset.GetNativeAsset().FcdURL != "" {
		return client.estimateGasFcd(ctx)
	}
	if client.Asset.GetNativeAsset().ChainGasPriceDefault > 0 {
		return xc.NewAmountBlockchainToMaskFloat64(client.Asset.GetNativeAsset().ChainGasPriceDefault), nil
	}
	return zero, errors.New("not implemented")
}

// RegisterEstimateGasCallback registers a callback to get gas price
func (client *Client) RegisterEstimateGasCallback(fn xc.EstimateGasFunc) {
	client.EstimateGasFunc = fn
}

// FetchBalance fetches balance for input asset for a Cosmos address
func (client *Client) FetchBalance(ctx context.Context, address xc.Address) (xc.AmountBlockchain, error) {
	if isNativeAsset(client.Asset.GetNativeAsset()) {
		return client.FetchNativeBalance(ctx, address)
	}
	_, err := types.GetFromBech32(client.Asset.GetNativeAsset().Contract, client.Prefix)
	if err != nil {
		// could be a custom denom.  Try querying as a native balance.
		return client.fetchBankModuleBalance(ctx, address, client.Asset)
	}
	return client.fetchContractBalance(ctx, address, client.Asset.GetAssetConfig().Contract)
}

func (client *Client) fetchContractBalance(ctx context.Context, address xc.Address, contractAddress string) (xc.AmountBlockchain, error) {
	zero := xc.NewAmountBlockchainFromUint64(0)

	_, err := types.GetFromBech32(string(address), client.Prefix)
	if err != nil {
		return zero, fmt.Errorf("bad address: '%v': %v", address, err)
	}

	input := json.RawMessage(`{"balance": {"address": "` + string(address) + `"}}`)
	balResp, err := wasmtypes.NewQueryClient(client.Ctx).SmartContractState(ctx, &wasmtypes.QuerySmartContractStateRequest{
		QueryData: wasmtypes.RawContractMessage(input),
		Address:   contractAddress,
	})
	if err != nil {
		return zero, fmt.Errorf("failed to get token balance: '%v': %v", address, err)
	}

	type TokenBalance struct {
		Balance string
	}
	var balResult TokenBalance
	err = json.Unmarshal(balResp.Data.Bytes(), &balResult)
	if err != nil {
		return zero, fmt.Errorf("failed to parse token balance: '%v': %v", address, err)
	}

	balance := xc.NewAmountBlockchainFromStr(balResult.Balance)
	return balance, nil
}

// FetchNativeBalance fetches account balance for a Cosmos address
func (client *Client) FetchNativeBalance(ctx context.Context, address xc.Address) (xc.AmountBlockchain, error) {
	return client.fetchBankModuleBalance(ctx, address, client.Asset)
}

// Cosmos chains can have multiple native assets.  This helper is necessary to query the
// native bank module for a given asset.
func (client *Client) fetchBankModuleBalance(ctx context.Context, address xc.Address, asset xc.ITask) (xc.AmountBlockchain, error) {
	zero := xc.NewAmountBlockchainFromUint64(0)

	_, err := types.GetFromBech32(string(address), client.Prefix)
	if err != nil {
		return zero, fmt.Errorf("bad address: '%v': %v", address, err)
	}
	denom := asset.GetNativeAsset().ChainCoin
	if denom == "" {
		if token, ok := asset.(*xc.TokenAssetConfig); ok {
			if token.Contract != "" {
				denom = token.Contract
			}
		}
	}
	if denom == "" {
		return zero, fmt.Errorf("failed to account balance: no denom on asset %s.%s", asset.GetAssetConfig().Asset, asset.GetNativeAsset().NativeAsset)
	}

	queryClient := banktypes.NewQueryClient(client.Ctx)
	balResp, err := queryClient.Balance(ctx, &banktypes.QueryBalanceRequest{
		Address: string(address),
		Denom:   denom,
	})
	if err != nil {
		if strings.Contains(err.Error(), "invalid denom") {
			// Some chains do not properly support getting balance by denom directly, but will support when getting all of the balances.
			allBals, err := queryClient.AllBalances(ctx, &banktypes.QueryAllBalancesRequest{
				Address: string(address),
				Pagination: &query.PageRequest{
					Limit: 100,
				},
			})
			if err != nil {
				return zero, fmt.Errorf("failed to get any account balance: '%v': %v", address, err)
			}
			for _, bal := range allBals.Balances {
				if bal.Denom == denom {
					return xc.AmountBlockchain(*bal.Amount.BigInt()), nil
				}
			}
		}
		return zero, fmt.Errorf("failed to get account balance: '%v': %v", address, err)
	}
	if balResp == nil || balResp.GetBalance() == nil {
		return zero, fmt.Errorf("failed to get account balance: '%v': %v", address, err)
	}
	balance := balResp.GetBalance().Amount.BigInt()
	return xc.AmountBlockchain(*balance), nil
}

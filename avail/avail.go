package avail

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/avail/internal/config"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"golang.org/x/crypto/sha3"

	gsrpc "github.com/centrifuge/go-substrate-rpc-client/v4"
	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"

	availTypes "github.com/0xPolygonHermez/zkevm-node/avail/types"
)

type HeaderRPCResponse struct {
	Result types.Header `json:"result"`
}

type DataProofRPCResponse struct {
	Result DataProof `json:"result"`
}

type DataProof struct {
	Root           string   `json:"root"`
	Proof          []string `json:"proof"`
	NumberOfLeaves uint     `json:"number_of_leaves"`
	LeafIndex      uint     `json:"leaf_index"`
	Leaf           string   `json:"leaf"`
}

func PostData(txData []byte) (*availTypes.BatchDAData, error) {
	var config config.Config

	err := config.GetConfig("/app/avail-config.json")
	if err != nil {
		return nil, fmt.Errorf("cannot get config:%w", err)
	}

	api, err := gsrpc.NewSubstrateAPI(config.ApiURL)
	if err != nil {
		return nil, fmt.Errorf("cannot get api:%w", err)
	}

	meta, err := api.RPC.State.GetMetadataLatest()
	if err != nil {
		return nil, fmt.Errorf("cannot get metadata:%w", err)
	}

	log.Infof("⚡️ Prepared data for Avail:%d bytes", len(txData))
	appID := 0

	// if app id is greater than 0 then it must be created before submitting data
	if config.AppID != 0 {
		appID = config.AppID
	}

	newCall, err := types.NewCall(meta, "DataAvailability.submit_data", types.NewBytes(txData))
	if err != nil {
		return nil, fmt.Errorf("cannot create new call:%w", err)
	}

	// Create the extrinsic
	ext := types.NewExtrinsic(newCall)

	genesisHash, err := api.RPC.Chain.GetBlockHash(0)
	if err != nil {
		return nil, fmt.Errorf("cannot get block hash:%w", err)
	}

	rv, err := api.RPC.State.GetRuntimeVersionLatest()
	if err != nil {
		return nil, fmt.Errorf("cannot get runtime version:%w", err)
	}

	keyringPair, err := signature.KeyringPairFromSecret(config.Seed, 42)
	if err != nil {
		return nil, fmt.Errorf("cannot create keypair:%w", err)
	}

	key, err := types.CreateStorageKey(meta, "System", "Account", keyringPair.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("cannot create storage key:%w", err)
	}

	var accountInfo types.AccountInfo
	ok, err := api.RPC.State.GetStorageLatest(key, &accountInfo)
	if err != nil || !ok {
		return nil, fmt.Errorf("cannot get latest storage:%w", err)
	}

	pendingExt, err := api.RPC.Author.PendingExtrinsics()
	if err != nil {
		return nil, fmt.Errorf("cannot get pending extrinsics:%w", err)
	}

	nonce := uint32(accountInfo.Nonce) + uint32(len(pendingExt))
	options := types.SignatureOptions{
		BlockHash:          genesisHash,
		Era:                types.ExtrinsicEra{IsMortalEra: false},
		GenesisHash:        genesisHash,
		Nonce:              types.NewUCompactFromUInt(uint64(nonce)),
		SpecVersion:        rv.SpecVersion,
		Tip:                types.NewUCompactFromUInt(5000),
		AppID:              types.NewUCompactFromUInt(uint64(appID)),
		TransactionVersion: rv.TransactionVersion,
	}

	err = ext.Sign(keyringPair, options)
	if err != nil {
		return nil, fmt.Errorf("cannot sign:%w", err)
	}

	// Send the extrinsic
	sub, err := api.RPC.Author.SubmitAndWatchExtrinsic(ext)
	if err != nil {
		return nil, fmt.Errorf("cannot submit extrinsic:%w", err)
	}

	defer sub.Unsubscribe()
	timeout := time.After(time.Duration(config.Timeout) * time.Second)
	var blockHash types.Hash
out:
	for {
		select {
		case status := <-sub.Chan():
			if status.IsInBlock {
				log.Infof("Extrinsic included in block %v", status.AsInBlock.Hex())
			}
			if status.IsFinalized {
				blockHash = status.AsFinalized
				break out
			} else if status.IsDropped {
				return nil, fmt.Errorf("❌ Extrinsic dropped")
			} else if status.IsUsurped {
				return nil, fmt.Errorf("❌ Extrinsic usurped")
			} else if status.IsInvalid {
				return nil, fmt.Errorf("❌ Extrinsic invalid")
			}
		case <-timeout:
			return nil, fmt.Errorf("⌛️ Timeout of %d seconds reached without getting finalized status for extrinsic", config.Timeout)
		}
	}

	log.Infof("✅ Data submitted by sequencer:%d bytes against AppID %v sent with hash %#x", len(txData), appID, blockHash)

	var dataProof DataProof
	var batchHash [32]byte
	maxTxIndex := 1
	h := sha3.NewLegacyKeccak256()
	h.Write(txData)
	h.Sum(batchHash[:0])

	for i := 0; i < maxTxIndex; i++ {
		resp, err := http.Post("https://kate.avail.tools/rpc", "application/json", strings.NewReader(fmt.Sprintf("{\"id\":1,\"jsonrpc\":\"2.0\",\"method\":\"kate_queryDataProof\",\"params\":[%d, \"%#x\"]}", i, blockHash)))
		if err != nil {
			return nil, fmt.Errorf("cannot post header request:%v", err)
		}
		defer resp.Body.Close()

		data, err := io.ReadAll(resp.Body)

		if err != nil {
			return nil, fmt.Errorf("cannot read body:%v", err)
		}

		var dataProofResp DataProofRPCResponse
		json.Unmarshal(data, &dataProofResp)

		if dataProofResp.Result.Leaf == fmt.Sprintf("%#x", batchHash) {
			dataProof = dataProofResp.Result
			break
		}

		maxTxIndex = int(dataProofResp.Result.NumberOfLeaves)
	}

	log.Infof("💿 received data proof:%+v", dataProof)
	var batchDAData availTypes.BatchDAData
	batchDAData.Proof = dataProof.Proof
	batchDAData.Width = dataProof.NumberOfLeaves
	batchDAData.LeafIndex = dataProof.LeafIndex

	header, err := api.RPC.Chain.GetHeader(blockHash)
	log.Infof("🎩 received header:%+v", header)

	batchDAData.BlockNumber = uint(header.Number)
	log.Infof("🟢 prepared DA data:%+v", batchDAData)

	if err != nil {
		return nil, fmt.Errorf("cannot get header:%+v", err)
	}

	destAddress, err := types.NewHashFromHexString(config.DestinationAddress)
	if err != nil {
		return nil, fmt.Errorf("cannot decode destination address:%w", err)
	}

	dispatchDataRootCall, err := types.NewCall(meta, "NomadDABridge.try_dispatch_data_root", types.NewUCompactFromUInt(uint64(config.DestinationDomain)), destAddress, types.Header(*header))

	if err != nil {
		return nil, fmt.Errorf("cannot create new call:%w", err)
	}

	dispatchDataRootExt := types.NewExtrinsic(dispatchDataRootCall)

	ok, err = api.RPC.State.GetStorageLatest(key, &accountInfo)
	if err != nil || !ok {
		return nil, fmt.Errorf("cannot get latest storage:%w", err)
	}

	pendingExt, err = api.RPC.Author.PendingExtrinsics()
	if err != nil {
		return nil, fmt.Errorf("cannot get pending extrinsics:%w", err)
	}

	nonce = uint32(accountInfo.Nonce) + uint32(len(pendingExt))
	options = types.SignatureOptions{
		BlockHash:          genesisHash,
		Era:                types.ExtrinsicEra{IsMortalEra: false},
		GenesisHash:        genesisHash,
		Nonce:              types.NewUCompactFromUInt(uint64(nonce)),
		SpecVersion:        rv.SpecVersion,
		Tip:                types.NewUCompactFromUInt(500),
		TransactionVersion: rv.TransactionVersion,
	}

	err = dispatchDataRootExt.Sign(keyringPair, options)
	if err != nil {
		return nil, fmt.Errorf("cannot sign:%w", err)
	}

	dispatchDataRootHash, err := api.RPC.Author.SubmitAndWatchExtrinsic(dispatchDataRootExt)
	if err != nil {
		return nil, fmt.Errorf("cannot dispatch data root:%w", err)
	}

	log.Infof("✅ Data root dispatched by sequencer with AppID %v sent with hash %#x\n", appID, dispatchDataRootHash)

	return &batchDAData, nil
}

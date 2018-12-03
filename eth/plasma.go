package eth

import (
	"context"
	"crypto/ecdsa"
	"errors"
	rootchain "github.com/FourthState/plasma-mvp-sidechain/contracts/wrappers"
	utxo "github.com/FourthState/plasma-mvp-sidechain/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/comparer"
	"github.com/syndtr/goleveldb/leveldb/memdb"
	"github.com/tendermint/tendermint/libs/log"
)

// Contains the binded wrapper and keys of the operator
type Plasma struct {
	session *rootchain.RootChainSession
	memdb   *memdb.DB
	db      *leveldb.DB
	logger  log.Logger
}

// InitPlasma binds the go wrapper to the deployed contract. This private key provides authentication
// for the operator
func InitPlasma(contractAddr string, client *Client, logger log.Logger, privateKey *ecdsa.PrivateKey, isValidator bool) (*Plasma, error) {
	plasmaContract, err := rootchain.NewRootChain(common.HexToAddress(contractAddr), client.ec)
	if err != nil {
		return nil, err
	}

	var plasmaSession *rootchain.RootChainSession
	if isValidator {
		// Create a session with the contract
		auth := bind.NewKeyedTransactor(privateKey)
		plasmaSession = &rootchain.RootChainSession{
			Contract: plasmaContract,
			CallOpts: bind.CallOpts{
				Pending: true,
			},
			TransactOpts: bind.TransactOpts{
				From:     auth.From,
				Signer:   auth.Signer,
				GasLimit: 3141592, // aribitrary. TODO: check this
			},
		}

	} else {
		// Create a session with the contract
		plasmaSession = &rootchain.RootChainSession{
			Contract: plasmaContract,
			CallOpts: bind.CallOpts{
				Pending: true,
			},
			TransactOpts: bind.TransactOpts{
				GasLimit: 3141592, // aribitrary. TODO: check this
			},
		}
	}

	plasma := &Plasma{
		session: plasmaSession,
		// capacity argument is advisory and not enforced in the memdb implementation
		// TODO: flush the in-memory DB to a local one to bound memory consumption
		memdb:  memdb.New(comparer.DefaultComparer, 1),
		logger: logger,
	}

	go plasma.watchDeposits()
	go plasma.watchExits()

	return plasma, nil
}

// SubmitBlock proxy
func (plasma *Plasma) SubmitBlock(header []byte) (*types.Transaction, error) {
	tx, err := plasma.session.SubmitBlock(header)
	if err != nil {
		return nil, err
	}

	return tx, nil
}

// CheckDeposit checks the existence of a deposit nonce
func (plasma *Plasma) CheckDeposit(nonce sdk.Uint) (utxo.Deposit, error) {
	key := prefixKey(depositPrefix, nonce.BigInt().Bytes())
	data, err := plasma.memdb.Get(key)

	// if entry exists, only return if we can decode successfully
	if err == nil {
		// try to decode and return
		var deposit utxo.Deposit
		err := rlp.DecodeBytes(data, &deposit)
		if err != nil {
			plasma.logger.Error("Error decoding cached deposit: %x", data)
		} else {
			return deposit, nil
		}
	}
	owner, amount, createdAt, err := plasma.session.GetDeposit(nonce.BigInt())
	if err != nil {
		plasma.logger.Error("Contract call, GetDeposit, failed %v", err)
		return utxo.Deposit{}, err
	}
	// deposit does not existed if the timestamp is the default solidity value
	if createdAt.Sign() == 0 {
		return utxo.Deposit{}, errors.New("deposit does not exist")
	}

	deposit := utxo.Deposit{
		Owner:  owner,
		Amount: amount.Int64(),
	}

	data, err = rlp.EncodeToBytes(deposit)
	if err != nil {
		plasma.logger.Error("Error encoding: %v", deposit)
		// force another contract call if this function is called again with the same nonce
		plasma.memdb.Delete(key)
	} else {
		// cache the deposit
		plasma.memdb.Put(key, data)
	}

	return deposit, nil
}

func (plasma *Plasma) CheckTransaction(position sdk.Uint) (bool, error) {
	key := prefixKey(transactionExitPrefix, position.BigInt().Bytes())

	return plasma.memdb.Contains(key), nil
}

func (plasma *Plasma) watchDeposits() {
	// suscribe to future deposits
	deposits := make(chan *rootchain.RootChainDeposit)
	opts := &bind.WatchOpts{
		Start:   nil, // latest block
		Context: context.Background(),
	}
	plasma.session.Contract.WatchDeposit(opts, deposits)

	for deposit := range deposits {
		key := prefixKey(depositPrefix, deposit.DepositNonce.Bytes())

		// remove the nonce, encode, and store
		val, err := rlp.EncodeToBytes(utxo.Deposit{
			Owner:  deposit.Depositor,
			Amount: deposit.Amount.Int64(),
		})

		if err != nil {
			plasma.logger.Error("Error encoding deposit event from contract: %v", deposit)
		} else {
			plasma.memdb.Put(key, val)
		}
	}
}

func (plasma *Plasma) watchExits() {
	startedDepositExits := make(chan *rootchain.RootChainStartedDepositExit)
	startedTransactionExits := make(chan *rootchain.RootChainStartedTransactionExit)

	opts := &bind.WatchOpts{
		Start:   nil, // latest block
		Context: context.Background(),
	}
	plasma.session.Contract.WatchStartedDepositExit(opts, startedDepositExits)
	plasma.session.Contract.WatchStartedTransactionExit(opts, startedTransactionExits)

	go func() {
		for depositExit := range startedDepositExits {
			nonce := depositExit.Nonce.Bytes()
			key := prefixKey(depositExitPrefix, nonce)
			plasma.memdb.Put(key, nil)
		}
	}()

	go func() {
		for transactionExit := range startedTransactionExits {
			position := transactionExit.Position.Bytes()
			key := prefixKey(transactionExitPrefix, position)
			plasma.memdb.Put(key, nil)
		}
	}()
}

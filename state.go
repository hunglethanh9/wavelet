package wavelet

import (
	"encoding/hex"
	"fmt"
	"github.com/perlin-network/graph/database"
	"github.com/perlin-network/life/exec"
	"github.com/perlin-network/wavelet/log"
	"github.com/phf/go-queue/queue"
	"github.com/pkg/errors"
	"io/ioutil"
	"path/filepath"
	"strings"
)

var (
	BucketAccounts = []byte("account_")
)

type state struct {
	*Ledger

	services []*service
}

// registerServicePath registers all the services in a path.
func (m *state) registerServicePath(path string) error {
	files, err := filepath.Glob(fmt.Sprintf("%s/*.wasm", path))
	if err != nil {
		return err
	}

	for _, f := range files {
		name := filepath.Base(f)

		if err := m.registerService(name[:len(name)-5], f); err != nil {
			return err
		}
		log.Info().Str("module", name).Msg("Registered transaction processor service.")
	}

	if len(m.services) == 0 {
		return errors.Errorf("No WebAssembly services were successfully registered for path: %s", path)
	}

	return nil
}

// registerService internally loads a *.wasm module representing a service, and registers the service
// with a specified name.
//
// Warning: will panic should there be errors in loading the service.
func (m *state) registerService(name string, path string) error {
	if !strings.HasSuffix(path, ".wasm") {
		return errors.Errorf("service code %s file should be in *.wasm format", path)
	}

	code, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}

	service := NewService(m, name)

	service.vm, err = exec.NewVirtualMachine(code, exec.VMConfig{
		DefaultMemoryPages: 128,
		DefaultTableSize:   65536,
	}, service, nil)

	if err != nil {
		return err
	}

	var exists bool

	service.entry, exists = service.vm.GetFunctionExport("process")
	if !exists {
		return errors.Errorf("could not find 'process' func in %s *.wasm file", path)
	}

	m.services = append(m.services, service)

	return nil
}

// applyTransaction runs a transaction, gets any transactions created by said transaction, and
// applies those transactions to the ledger state.
func (s *state) applyTransaction(tx *database.Transaction) error {
	pending := queue.New()
	pending.PushBack(tx)

	for pending.Len() > 0 {
		tx := pending.PopFront().(*database.Transaction)

		new, err := s.doApplyTransaction(tx)
		if err != nil {
			return err
		}

		for _, tx := range new {
			pending.PushBack(tx)
		}
	}

	return nil
}

// doApplyTransaction runs a transaction through a transaction processor and applies its recorded
// changes to the ledger state.
//
// Any additional transactions that are recursively generated by smart contracts for example are returned.
func (s *state) doApplyTransaction(tx *database.Transaction) ([]*database.Transaction, error) {
	senderID, err := hex.DecodeString(tx.Sender)
	if err != nil {
		return nil, err
	}

	// Should the transaction be a nop simply increment the accounts nonce.
	if tx.Tag == "nop" {
		account, err := s.LoadAccount(senderID)
		if err != nil {
			return nil, errors.Errorf("nop transaction sender account %s does not exist", tx.Sender)
		}

		account.Nonce++

		s.SaveAccount(account, nil)

		return nil, nil
	}

	var deltas []*Delta

	// Iterate through all registered services and run them on the transactions given their tags and payload.
	var pendingTransactions []*database.Transaction

	for _, service := range s.services {
		deltas, pending, err := service.Run(tx)

		if err != nil {
			return nil, err
		}

		deltas = append(deltas, deltas...)

		if len(pending) > 0 {
			pendingTransactions = append(pendingTransactions, pending...)
		}
	}

	// List of accounts which have been modified in the ledger.
	accounts := make(map[string]*Account)

	// Load the senders account.
	sender, err := s.LoadAccount(senderID)
	if err != nil {
		if tx.Nonce == 0 {
			sender = NewAccount(senderID)
		} else {
			return nil, errors.Errorf("transaction sender account %s does not exist", tx.Sender)
		}
	}

	accounts[writeString(senderID)] = sender

	accountDeltas := make(map[string][]*Delta)

	for _, change := range deltas {
		accountID := writeString(change.Account)

		account, exists := accounts[accountID]

		if !exists {
			account, err = s.LoadAccount(change.Account)
			if err != nil {
				account = NewAccount(change.Account)
			}

			accounts[accountID] = account
		}

		account.State, _ = account.State.Store(change.Key, change.NewValue)
		accountDeltas[accountID] = append(accountDeltas[accountID], change)
	}

	// Increment the senders account nonce.
	accounts[writeString(senderID)].Nonce++

	// Save all modified accounts to the ledger.
	for id, account := range accounts {
		s.SaveAccount(account, accountDeltas[id])
	}

	return pendingTransactions, nil
}

// LoadAccount reads the account data for a given hex public key.
func (s *state) LoadAccount(key []byte) (*Account, error) {
	bytes, err := s.Get(merge(BucketAccounts, key))
	if err != nil {
		return nil, errors.Wrapf(err, "account %s not found in ledger state", key)
	}

	account := NewAccount(key)
	err = account.Unmarshal(bytes)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to decode account bytes")
	}

	return account.Clone(), nil
}

func (s *state) SaveAccount(account *Account, deltas []*Delta) error {
	err := s.Put(merge(BucketAccounts, account.PublicKey), account.MarshalBinary())
	if err != nil {
		return err
	}

	// TODO: Report deltas.

	return nil
}

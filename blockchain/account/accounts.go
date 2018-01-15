// Package account stores and tracks accounts within a Chain Core.
package account

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"sync"
	"time"

	"github.com/golang/groupcache/lru"
	log "github.com/sirupsen/logrus"
	dbm "github.com/tendermint/tmlibs/db"

	"github.com/bytom/blockchain/pseudohsm"
	"github.com/bytom/blockchain/signers"
	"github.com/bytom/blockchain/txbuilder"
	"github.com/bytom/common"
	"github.com/bytom/consensus"
	"github.com/bytom/crypto"
	"github.com/bytom/crypto/ed25519/chainkd"
	"github.com/bytom/crypto/sha3pool"
	"github.com/bytom/errors"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/vm/vmutil"
)

const (
	maxAccountCache = 1000
	aliasPrefix     = "ALI:"
	accountPrefix   = "ACC:"
	accountCPPrefix = "ACP:"
	keyNextIndex    = "NextIndex"
	indexPrefix     = "ACIDX:"
	purpose         = 0x8000002C
	harden          = 0x80000000
)

// pre-define errors for supporting bytom errorFormatter
var (
	ErrDuplicateAlias = errors.New("duplicate account alias")
	ErrFindAccount    = errors.New("fail to find account")
	ErrMarshalAccount = errors.New("failed marshal account")
	ErrMarshalTags    = errors.New("failed marshal account to update tags")
	ErrStandardQuorum = errors.New("need single key pair account to create standard transaction")
)

func aliasKey(name string) []byte {
	return []byte(aliasPrefix + name)
}

func indexKey(xpub chainkd.XPub) []byte {
	return []byte(indexPrefix + xpub.String())
}

//Key account store prefix
func Key(name string) []byte {
	return []byte(accountPrefix + name)
}

//CPKey account control promgram store prefix
func CPKey(hash common.Hash) []byte {
	return append([]byte(accountCPPrefix), hash[:]...)
}

// NewManager creates a new account manager
func NewManager(walletDB dbm.DB, chain *protocol.Chain) *Manager {
	var nextIndex uint64
	if index := walletDB.Get([]byte(keyNextIndex)); index != nil {
		nextIndex = uint64(binary.LittleEndian.Uint64(index))
	}
	return &Manager{
		db:           walletDB,
		chain:        chain,
		utxoDB:       newReserver(chain, walletDB),
		cache:        lru.New(maxAccountCache),
		aliasCache:   lru.New(maxAccountCache),
		delayedACPs:  make(map[*txbuilder.TemplateBuilder][]*CtrlProgram),
		acpIndexNext: nextIndex,
	}
}

// Manager stores accounts and their associated control programs.
type Manager struct {
	db     dbm.DB
	chain  *protocol.Chain
	utxoDB *reserver

	cacheMu    sync.Mutex
	cache      *lru.Cache
	aliasCache *lru.Cache

	delayedACPsMu sync.Mutex
	delayedACPs   map[*txbuilder.TemplateBuilder][]*CtrlProgram

	acpMu        sync.Mutex
	acpIndexNext uint64 // next acp index in our block
	acpIndexCap  uint64 // points to end of block
}

// ExpireReservations removes reservations that have expired periodically.
// It blocks until the context is canceled.
func (m *Manager) ExpireReservations(ctx context.Context, period time.Duration) {
	ticks := time.Tick(period)
	for {
		select {
		case <-ctx.Done():
			log.Info("Deposed, ExpireReservations exiting")
			return
		case <-ticks:
			err := m.utxoDB.ExpireReservations(ctx)
			if err != nil {
				log.WithField("error", err).Error("Expire reservations")
			}
		}
	}
}

// Account is structure of Bytom account
type Account struct {
	*signers.Signer
	ID           string                 `json:"id"`
	Alias        string                 `json:"alias"`
	Tags         map[string]interface{} `json:"tags"`
	AccountXPubs []chainkd.XPub         `json:"account_xpubs"`
}

func (m *Manager) getNextAccountIndex(xpubs []chainkd.XPub) (*uint32, error) {
	var nextIndex uint32 = 1

	if rawIndex := m.db.Get(indexKey(xpubs[0])); rawIndex != nil {
		nextIndex = uint32(binary.LittleEndian.Uint32(rawIndex)) + 1
	}

	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, nextIndex)
	m.db.Set(indexKey(xpubs[0]), buf)

	return &nextIndex, nil
}

// path returns the complete path for hardened derived keys for new account
// account path format /Purpose'/index'
func accountPath(index uint32) [][]byte {
	var path [][]byte

	purposePath := make([]byte, 4)
	binary.LittleEndian.PutUint32(purposePath[:], purpose)
	path = append(path, purposePath[:])
	indexPath := make([]byte, 4)
	binary.LittleEndian.PutUint32(indexPath[:], harden+index)
	path = append(path, indexPath)

	return path
}

// path returns the complete path for derived keys
// path format purpose'/account'/change/index
func path(accountIndex uint32, change bool, itemIndexes ...uint64) [][]byte {
	var path [][]byte
	var purposeBytes [4]byte
	binary.LittleEndian.PutUint32(purposeBytes[:], purpose)
	path = append(path, purposeBytes[:])

	var accountIndexBytes [4]byte
	binary.LittleEndian.PutUint32(accountIndexBytes[:], harden+accountIndex)
	path = append(path, accountIndexBytes[:])

	changePath := make([]byte, 1)
	if change == true {
		changePath[0] = 1
	} else {
		changePath[0] = 0
	}
	path = append(path, changePath[:])

	for _, idx := range itemIndexes {
		var idxBytes [8]byte
		binary.LittleEndian.PutUint64(idxBytes[:], idx)
		path = append(path, idxBytes[:])
	}
	return path
}

// Create creates a new Account.
func (m *Manager) Create(ctx context.Context, hsm *pseudohsm.HSM, xpubs []chainkd.XPub, quorum int, alias string, tags map[string]interface{}, accessToken, auth string) (*Account, error) {
	if existed := m.db.Get(aliasKey(alias)); existed != nil {
		return nil, ErrDuplicateAlias
	}

	nextAccountIndex, err := m.getNextAccountIndex(xpubs)
	if err != nil {
		return nil, errors.Wrap(err, "get account index error")
	}

	path := accountPath(*nextAccountIndex)
	xpriv, err := hsm.LoadChainKDKey(xpubs[0], auth)
	if err != nil {
		return nil, err
	}

	//derived account hardened private key and store
	accountXPriv := xpriv.Derive(path, true)
	accountXPub := accountXPriv.XPub()

	accountXPubs := []chainkd.XPub{accountXPub}

	id, signer, err := signers.Create(ctx, m.db, "account", xpubs, quorum, *nextAccountIndex, accessToken)
	if err != nil {
		return nil, errors.Wrap(err)
	}
	account := &Account{Signer: signer, ID: id, Alias: alias, Tags: tags, AccountXPubs: accountXPubs}
	rawAccount, err := json.Marshal(account)
	if err != nil {
		return nil, ErrMarshalAccount
	}

	storeBatch := m.db.NewBatch()
	accountID := Key(id)
	storeBatch.Set(accountID, rawAccount)
	storeBatch.Set(aliasKey(alias), []byte(id))
	storeBatch.Write()

	return account, nil
}

// UpdateTags modifies the tags of the specified account. The account may be
// identified either by ID or Alias, but not both.
func (m *Manager) UpdateTags(ctx context.Context, accountInfo string, tags map[string]interface{}) (err error) {
	account := &Account{}
	if account, err = m.FindByAlias(nil, accountInfo); err != nil {
		if account, err = m.findByID(ctx, accountInfo); err != nil {
			return err
		}
	}

	account.Tags = tags
	rawAccount, err := json.Marshal(account)
	if err != nil {
		return ErrMarshalTags
	}

	m.db.Set(Key(account.ID), rawAccount)
	m.cacheMu.Lock()
	m.cache.Add(account.ID, account)
	m.cacheMu.Unlock()
	return nil
}

// FindByAlias retrieves an account's Signer record by its alias
func (m *Manager) FindByAlias(ctx context.Context, alias string) (*Account, error) {
	m.cacheMu.Lock()
	cachedID, ok := m.aliasCache.Get(alias)
	m.cacheMu.Unlock()
	if ok {
		return m.findByID(ctx, cachedID.(string))
	}

	rawID := m.db.Get(aliasKey(alias))
	if rawID == nil {
		return nil, ErrFindAccount
	}

	accountID := string(rawID)
	m.cacheMu.Lock()
	m.aliasCache.Add(alias, accountID)
	m.cacheMu.Unlock()
	return m.findByID(ctx, accountID)
}

// findByID returns an account's Signer record by its ID.
func (m *Manager) findByID(ctx context.Context, id string) (*Account, error) {
	m.cacheMu.Lock()
	cachedAccount, ok := m.cache.Get(id)
	m.cacheMu.Unlock()
	if ok {
		return cachedAccount.(*Account), nil
	}

	rawAccount := m.db.Get(Key(id))
	if rawAccount == nil {
		return nil, ErrFindAccount
	}

	account := &Account{}
	if err := json.Unmarshal(rawAccount, account); err != nil {
		return nil, err
	}

	m.cacheMu.Lock()
	m.cache.Add(id, account)
	m.cacheMu.Unlock()
	return account, nil
}

// GetAliasByID return the account alias by given ID
func (m *Manager) GetAliasByID(id string) string {
	account := &Account{}

	rawAccount := m.db.Get(Key(id))
	if rawAccount == nil {
		log.Warn("fail to find account")
		return ""
	}

	if err := json.Unmarshal(rawAccount, account); err != nil {
		log.Warn(err)
		return ""
	}
	return account.Alias
}

// CreateAddress generate an address for the select account
func (m *Manager) CreateAddress(ctx context.Context, accountID string, change bool) (cp *CtrlProgram, err error) {
	account, err := m.findByID(ctx, accountID)
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(defaultReceiverExpiry)
	if len(account.XPubs) == 1 {
		cp, err = m.createP2PKH(ctx, account, change, expiresAt)
	} else {
		cp, err = m.createP2SH(ctx, account, change, expiresAt)
	}
	if err != nil {
		return nil, err
	}

	if err = m.insertAccountControlProgram(ctx, cp); err != nil {
		return nil, err
	}
	return cp, nil
}

// CPPath returns the complete path for control program derived keys
// path format /change/index
func CPPath(change bool, itemIndexes ...uint64) [][]byte {
	var path [][]byte
	changePath := make([]byte, 1)
	if change == true {
		changePath[0] = 1
	} else {
		changePath[0] = 0
	}
	path = append(path, changePath[:])

	for _, idx := range itemIndexes {
		var idxBytes [8]byte
		binary.LittleEndian.PutUint64(idxBytes[:], idx)
		path = append(path, idxBytes[:])
	}
	return path
}

func (m *Manager) createP2PKH(ctx context.Context, account *Account, change bool, expiresAt time.Time) (*CtrlProgram, error) {
	idx, err := m.nextCPIndex(m.db, account.KeyIndex, change)
	path := CPPath(change, idx)
	derivedXPubs := chainkd.DeriveXPubs(account.AccountXPubs, path)
	derivedPK := derivedXPubs[0].PublicKey()
	pubHash := crypto.Ripemd160(derivedPK)

	// TODO: pass different params due to config
	address, err := common.NewAddressWitnessPubKeyHash(pubHash, &consensus.MainNetParams)
	if err != nil {
		return nil, err
	}

	control, err := vmutil.P2PKHSigProgram([]byte(pubHash))
	if err != nil {
		return nil, err
	}

	return &CtrlProgram{
		AccountID:      account.ID,
		Address:        address.EncodeAddress(),
		KeyIndex:       idx,
		ControlProgram: control,
		Change:         change,
		ExpiresAt:      expiresAt,
	}, nil
}

func (m *Manager) createP2SH(ctx context.Context, account *Account, change bool, expiresAt time.Time) (*CtrlProgram, error) {
	idx := m.nextIndex()
	path := CPPath(change, idx)
	derivedXPubs := chainkd.DeriveXPubs(account.AccountXPubs, path)
	derivedPKs := chainkd.XPubKeys(derivedXPubs)
	signScript, err := vmutil.P2SPMultiSigProgram(derivedPKs, account.Quorum)
	if err != nil {
		return nil, err
	}
	scriptHash := crypto.Sha256(signScript)

	// TODO: pass different params due to config
	address, err := common.NewAddressWitnessScriptHash(scriptHash, &consensus.MainNetParams)
	if err != nil {
		return nil, err
	}

	control, err := vmutil.P2SHProgram(scriptHash)
	if err != nil {
		return nil, err
	}

	return &CtrlProgram{
		AccountID:      account.ID,
		Address:        address.EncodeAddress(),
		KeyIndex:       idx,
		ControlProgram: control,
		Change:         change,
		ExpiresAt:      expiresAt,
	}, nil
}

func (m *Manager) createControlProgram(ctx context.Context, accountID string, change bool, expiresAt time.Time) (*CtrlProgram, error) {
	account, err := m.findByID(ctx, accountID)
	if err != nil {
		return nil, err
	}

	idx, err := m.nextCPIndex(m.db, account.KeyIndex, change)
	if err != nil {
		return nil, err
	}

	path := CPPath(change, idx)
	derivedXPubs := chainkd.DeriveXPubs(account.AccountXPubs, path)
	derivedPKs := chainkd.XPubKeys(derivedXPubs)
	control, err := vmutil.P2SPMultiSigProgram(derivedPKs, account.Quorum)
	if err != nil {
		return nil, err
	}
	return &CtrlProgram{
		AccountID:      account.ID,
		KeyIndex:       idx,
		ControlProgram: control,
		Change:         change,
		ExpiresAt:      expiresAt,
	}, nil
}

// CreateControlProgram creates a control program
// that is tied to the Account and stores it in the database.
func (m *Manager) CreateControlProgram(ctx context.Context, accountID string, change bool, expiresAt time.Time) ([]byte, error) {
	cp, err := m.createControlProgram(ctx, accountID, change, expiresAt)
	if err != nil {
		return nil, err
	}

	if err = m.insertAccountControlProgram(ctx, cp); err != nil {
		return nil, err
	}
	return cp.ControlProgram, nil
}

//CtrlProgram is structure of account control program
type CtrlProgram struct {
	AccountID      string
	Address        string
	KeyIndex       uint64
	ControlProgram []byte
	Change         bool
	ExpiresAt      time.Time
}

func (m *Manager) insertAccountControlProgram(ctx context.Context, progs ...*CtrlProgram) error {
	var hash common.Hash
	for _, prog := range progs {
		accountCP, err := json.Marshal(prog)
		if err != nil {
			return err
		}

		sha3pool.Sum256(hash[:], prog.ControlProgram)
		m.db.Set(CPKey(hash), accountCP)
	}
	return nil
}

// GetCoinbaseControlProgram will return a coinbase script
func (m *Manager) GetCoinbaseControlProgram(height uint64) ([]byte, error) {
	accountIter := m.db.IteratorPrefix([]byte(accountPrefix))
	defer accountIter.Release()
	if !accountIter.Next() {
		log.Warningf("GetCoinbaseControlProgram: can't find any account in db")
		return vmutil.CoinbaseProgram(nil, 0, height)
	}
	rawAccount := accountIter.Value()
	account := &Account{}
	if err := json.Unmarshal(rawAccount, account); err != nil {
		log.Errorf("GetCoinbaseControlProgram: fail to unmarshal account %v", err)
		return vmutil.CoinbaseProgram(nil, 0, height)
	}

	idx, err := m.nextCoinbaseIndex(nil, account.Signer.KeyIndex, false)
	if err != nil {
		log.Errorf("GetCoinbaseControlProgram: fail to get nextIndex %v", err)
		return vmutil.CoinbaseProgram(nil, 0, height)
	}

	path := CPPath(false, idx)
	derivedXPubs := chainkd.DeriveXPubs(account.AccountXPubs, path)
	derivedPKs := chainkd.XPubKeys(derivedXPubs)
	script, err := vmutil.CoinbaseProgram(derivedPKs, account.Quorum, height)
	if err != nil {
		return script, err
	}

	err = m.insertAccountControlProgram(nil, &CtrlProgram{
		AccountID:      account.ID,
		KeyIndex:       idx,
		ControlProgram: script,
		Change:         false,
	})
	if err != nil {
		log.Errorf("GetCoinbaseControlProgram: fail to insertAccountControlProgram %v", err)
	}
	return script, nil
}

func (m *Manager) nextIndex() uint64 {
	m.acpMu.Lock()
	defer m.acpMu.Unlock()

	n := m.acpIndexNext
	m.acpIndexNext++
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, m.acpIndexNext)
	m.db.Set([]byte(keyNextIndex), buf)
	return n
}

func (m *Manager) nextCPIndex(db dbm.DB, accountIndex uint32, change bool) (uint64, error) {
	m.acpMu.Lock()
	defer m.acpMu.Unlock()

	key := make([]byte, 0)

	accountIndexPath := make([]byte, 4)
	binary.LittleEndian.PutUint32(accountIndexPath[:], accountIndex)
	key = append(key, accountIndexPath[:]...)

	changePath := make([]byte, 2)
	if change {
		binary.LittleEndian.PutUint16(changePath[:], 0x01)
	} else {
		binary.LittleEndian.PutUint16(changePath[:], 0x00)
	}
	key = append(key, changePath[:]...)

	var nextIndex uint64 = 1
	if rawIndex := db.Get(key); rawIndex != nil {
		nextIndex = uint64(binary.LittleEndian.Uint64(rawIndex)) + 1
	}

	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, nextIndex)
	db.Set(key, buf)

	return nextIndex, nil
}

func (m *Manager) nextCoinbaseIndex(ctx context.Context, accountIndex uint32, change bool) (uint64, error) {
	n, err := m.nextCPIndex(m.db, accountIndex, change)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteAccount deletes the account's ID or alias matching accountInfo.
func (m *Manager) DeleteAccount(in struct {
	AccountInfo string `json:"account_info"`
}) (err error) {
	account := &Account{}
	if account, err = m.FindByAlias(nil, in.AccountInfo); err != nil {
		if account, err = m.findByID(nil, in.AccountInfo); err != nil {
			return err
		}
	}

	storeBatch := m.db.NewBatch()
	m.aliasCache.Remove(account.Alias)
	storeBatch.Delete(aliasKey(account.Alias))
	storeBatch.Delete(Key(account.ID))
	storeBatch.Write()
	return nil
}

// ListAccounts will return the accounts in the db
func (m *Manager) ListAccounts(id string) ([]*Account, error) {
	accounts := []*Account{}
	accountIter := m.db.IteratorPrefix([]byte(accountPrefix + id))
	defer accountIter.Release()

	for accountIter.Next() {
		account := &Account{}
		if err := json.Unmarshal(accountIter.Value(), &account); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}

	return accounts, nil
}

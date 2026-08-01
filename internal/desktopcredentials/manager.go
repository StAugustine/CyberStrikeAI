package desktopcredentials

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"cyberstrike-ai/internal/config"
	"gopkg.in/yaml.v3"
)

const ReferencePrefix = "keyring://"

const (
	PathOpenAI             = "openai.api_key"
	PathVision             = "vision.api_key"
	PathFOFA               = "fofa.api_key"
	PathZoomEye            = "zoomeye.api_key"
	PathQuake              = "quake.api_key"
	PathShodan             = "shodan.api_key"
	PathHitlAuditModel     = "hitl.audit_model.api_key"
	PathKnowledgeEmbedding = "knowledge.embedding.api_key"
	PathKnowledgeRerank    = "knowledge.retrieval.rerank.api_key"
)

type Store interface {
	Get(account string) (string, error)
	Set(account, secret string) error
	Delete(account string) error
}

type accountGenerator func() (string, error)

type Manager struct {
	store      Store
	newAccount accountGenerator
	mu         sync.RWMutex
	references map[string]string
}

type ConfigCredentialInspection struct {
	PlaintextPaths []string
	ReferencePaths []string
}

// InspectConfigCredentials reports credential locations without returning
// secret values or keyring account identifiers.
func InspectConfigCredentials(cfg *config.Config) (ConfigCredentialInspection, error) {
	inspection := ConfigCredentialInspection{
		PlaintextPaths: []string{},
		ReferencePaths: []string{},
	}
	err := visitSecretSlots(cfg, func(slot secretSlot) error {
		value := strings.TrimSpace(slot.get())
		if value == "" {
			return nil
		}
		_, isReference, err := ParseReference(value)
		if err != nil {
			return fmt.Errorf("inspect desktop credential %s: %w", slot.path, err)
		}
		if isReference {
			inspection.ReferencePaths = append(inspection.ReferencePaths, slot.path)
		} else {
			inspection.PlaintextPaths = append(inspection.PlaintextPaths, slot.path)
		}
		return nil
	})
	if err != nil {
		return ConfigCredentialInspection{}, err
	}
	return inspection, nil
}

func NewManager(store Store) (*Manager, error) {
	return newManager(store, randomAccount)
}

func newManager(store Store, generator accountGenerator) (*Manager, error) {
	if store == nil {
		return nil, errors.New("desktop credential store is required")
	}
	if generator == nil {
		return nil, errors.New("desktop credential account generator is required")
	}
	return &Manager{
		store:      store,
		newAccount: generator,
		references: make(map[string]string),
	}, nil
}

func AIChannelPath(id string) string {
	return "ai.channels." + config.NormalizeAIChannelID(id) + ".api_key"
}

func Reference(account string) (string, error) {
	account = strings.TrimSpace(account)
	if !validAccount(account) {
		return "", errors.New("invalid desktop credential account")
	}
	return ReferencePrefix + account, nil
}

func ParseReference(value string) (account string, isReference bool, err error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, ReferencePrefix) {
		return "", false, nil
	}
	account = strings.TrimPrefix(value, ReferencePrefix)
	if !validAccount(account) {
		return "", true, errors.New("invalid desktop credential reference")
	}
	return account, true, nil
}

// ResolveAndMigrate resolves existing references into the runtime config and,
// after explicit approval, moves legacy plaintext values into the platform
// credential store. Plaintext is replaced on disk only after every credential
// write succeeds.
func (m *Manager) ResolveAndMigrate(
	cfg *config.Config,
	approve func(paths []string) error,
	persist func(*config.Config) error,
) error {
	if cfg == nil {
		return errors.New("desktop config is required")
	}
	persisted, err := cloneConfig(cfg)
	if err != nil {
		return err
	}
	persistedSlots, err := indexedSecretSlots(persisted)
	if err != nil {
		return err
	}

	resolvedReferences := make(map[string]string)
	openAIAliasPath, openAIIsAlias := defaultAIChannelAlias(cfg)
	type plaintextCredential struct {
		path          string
		value         string
		persistedSlot secretSlot
	}
	plaintextCredentials := make([]plaintextCredential, 0)
	createdAccounts := make([]string, 0)
	rollback := func() {
		for _, account := range createdAccounts {
			_ = m.store.Delete(account)
		}
	}

	err = visitSecretSlots(cfg, func(slot secretSlot) error {
		value := strings.TrimSpace(slot.get())
		if value == "" {
			return nil
		}
		account, isReference, parseErr := ParseReference(value)
		if parseErr != nil {
			return fmt.Errorf("resolve desktop credential %s: %w", slot.path, parseErr)
		}
		if isReference {
			secret, getErr := m.store.Get(account)
			if getErr != nil {
				return fmt.Errorf("read desktop credential %s: %w", slot.path, getErr)
			}
			if strings.TrimSpace(secret) == "" {
				return fmt.Errorf("read desktop credential %s: stored value is empty", slot.path)
			}
			slot.set(secret)
			resolvedReferences[slot.path] = value
			return nil
		}

		persistedSlot, ok := persistedSlots[slot.path]
		if !ok {
			return fmt.Errorf("persist desktop credential %s: config path is missing", slot.path)
		}
		plaintextCredentials = append(plaintextCredentials, plaintextCredential{
			path:          slot.path,
			value:         value,
			persistedSlot: persistedSlot,
		})
		return nil
	})
	if err != nil {
		return err
	}
	if openAIIsAlias {
		for id, channel := range cfg.AI.Channels {
			if AIChannelPath(id) == openAIAliasPath {
				cfg.OpenAI.APIKey = channel.APIKey
				break
			}
		}
	}

	sort.Slice(plaintextCredentials, func(i, j int) bool {
		return plaintextCredentials[i].path < plaintextCredentials[j].path
	})
	if len(plaintextCredentials) != 0 {
		if approve == nil {
			return errors.New("desktop credential migration requires explicit approval")
		}
		paths := make([]string, len(plaintextCredentials))
		for index, credential := range plaintextCredentials {
			paths[index] = credential.path
		}
		if err := approve(paths); err != nil {
			return fmt.Errorf("approve desktop credential migration: %w", err)
		}
	}

	for _, credential := range plaintextCredentials {
		account, accountErr := m.newAccount()
		if accountErr != nil {
			rollback()
			return fmt.Errorf("create desktop credential account for %s: %w", credential.path, accountErr)
		}
		reference, referenceErr := Reference(account)
		if referenceErr != nil {
			rollback()
			return fmt.Errorf("create desktop credential reference for %s: %w", credential.path, referenceErr)
		}
		if setErr := m.store.Set(account, credential.value); setErr != nil {
			rollback()
			return fmt.Errorf("store desktop credential %s: %w", credential.path, setErr)
		}
		createdAccounts = append(createdAccounts, account)
		credential.persistedSlot.set(reference)
		resolvedReferences[credential.path] = reference
	}
	if openAIIsAlias && strings.TrimSpace(cfg.OpenAI.APIKey) != "" {
		reference := resolvedReferences[openAIAliasPath]
		if reference == "" {
			rollback()
			return fmt.Errorf("persist desktop credential %s: default AI channel is not protected", PathOpenAI)
		}
		persisted.OpenAI.APIKey = reference
	}
	if len(plaintextCredentials) != 0 {
		if persist == nil {
			rollback()
			return errors.New("desktop credential migration requires a persistence callback")
		}
		if err := persist(persisted); err != nil {
			rollback()
			return fmt.Errorf("persist desktop credential references: %w", err)
		}
	}

	m.mu.Lock()
	m.references = resolvedReferences
	m.mu.Unlock()
	return nil
}

// PersistenceCopy returns a deep copy whose secret values are credential
// references. It fails closed if a runtime secret has no protected reference.
func (m *Manager) PersistenceCopy(cfg *config.Config) (*config.Config, error) {
	if cfg == nil {
		return nil, errors.New("desktop config is required")
	}
	openAIAliasPath, openAIIsAlias := defaultAIChannelAlias(cfg)
	copyConfig, err := cloneConfig(cfg)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	references := cloneReferences(m.references)
	m.mu.RUnlock()
	if err := visitSecretSlots(copyConfig, func(slot secretSlot) error {
		if strings.TrimSpace(slot.get()) == "" {
			return nil
		}
		reference := references[slot.path]
		if reference == "" {
			return fmt.Errorf("desktop credential %s is not protected", slot.path)
		}
		slot.set(reference)
		return nil
	}); err != nil {
		return nil, err
	}
	if openAIIsAlias && strings.TrimSpace(copyConfig.OpenAI.APIKey) != "" {
		reference := references[openAIAliasPath]
		if reference == "" {
			return nil, fmt.Errorf("desktop credential %s is not protected", PathOpenAI)
		}
		copyConfig.OpenAI.APIKey = reference
	}
	return copyConfig, nil
}

func (m *Manager) RedactedCopy(cfg *config.Config) (*config.Config, error) {
	if cfg == nil {
		return nil, errors.New("desktop config is required")
	}
	copyConfig, err := cloneConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := visitSecretSlots(copyConfig, func(slot secretSlot) error {
		slot.set("")
		return nil
	}); err != nil {
		return nil, err
	}
	copyConfig.OpenAI.APIKey = ""
	return copyConfig, nil
}

// ConfiguredPaths reports only whether each managed field has a protected
// credential. It never returns references, accounts, or secret values.
func (m *Manager) ConfiguredPaths(cfg *config.Config) (map[string]bool, error) {
	if cfg == nil {
		return nil, errors.New("desktop config is required")
	}
	m.mu.RLock()
	references := cloneReferences(m.references)
	m.mu.RUnlock()
	status := make(map[string]bool)
	if err := visitSecretSlots(cfg, func(slot secretSlot) error {
		status[slot.path] = strings.TrimSpace(slot.get()) != "" && references[slot.path] != ""
		return nil
	}); err != nil {
		return nil, err
	}
	if aliasPath, aliasesDefault := defaultAIChannelAlias(cfg); aliasesDefault {
		status[PathOpenAI] = status[aliasPath]
	}
	return status, nil
}

func (m *Manager) BeginUpdate() *Update {
	return &Update{
		manager:  m,
		staged:   make(map[string]string),
		previous: make(map[string]string),
	}
}

type Update struct {
	manager         *Manager
	staged          map[string]string
	previous        map[string]string
	createdAccounts []string
	active          bool
	finished        bool
}

// Protect applies desktop update semantics to one secret: an empty incoming
// value preserves the current secret, while a new value is written to a fresh
// credential account and staged for the next config save.
func (u *Update) Protect(path string, incoming *string, current string) error {
	if u == nil || u.manager == nil {
		return errors.New("desktop credential update is required")
	}
	if u.active || u.finished {
		return errors.New("desktop credential update is no longer mutable")
	}
	path = strings.TrimSpace(path)
	if path == "" || incoming == nil {
		return errors.New("desktop credential update path and value are required")
	}
	value := strings.TrimSpace(*incoming)
	if value == "" {
		*incoming = current
		return nil
	}
	if value == current {
		*incoming = current
		return nil
	}
	if _, isReference, err := ParseReference(value); isReference || err != nil {
		return fmt.Errorf("desktop credential %s must be supplied as a secret value", path)
	}
	if previousStaged := u.staged[path]; previousStaged != "" {
		if account, ok, _ := ParseReference(previousStaged); ok {
			_ = u.manager.store.Delete(account)
		}
	}
	account, err := u.manager.newAccount()
	if err != nil {
		return fmt.Errorf("create desktop credential account for %s: %w", path, err)
	}
	reference, err := Reference(account)
	if err != nil {
		return fmt.Errorf("create desktop credential reference for %s: %w", path, err)
	}
	if err := u.manager.store.Set(account, value); err != nil {
		return fmt.Errorf("store desktop credential %s: %w", path, err)
	}
	u.createdAccounts = append(u.createdAccounts, account)
	u.staged[path] = reference
	*incoming = value
	return nil
}

func (u *Update) Activate() error {
	if u == nil || u.manager == nil {
		return errors.New("desktop credential update is required")
	}
	if u.finished {
		return errors.New("desktop credential update is already finished")
	}
	if u.active {
		return nil
	}
	u.manager.mu.Lock()
	for path, reference := range u.staged {
		u.previous[path] = u.manager.references[path]
		u.manager.references[path] = reference
	}
	u.manager.mu.Unlock()
	u.active = true
	return nil
}

func (u *Update) Commit() {
	if u == nil || u.manager == nil || u.finished {
		return
	}
	if !u.active {
		_ = u.Activate()
	}
	u.finished = true
	for path, previous := range u.previous {
		if previous == "" || previous == u.staged[path] || u.manager.referenceInUse(previous) {
			continue
		}
		if account, ok, _ := ParseReference(previous); ok {
			_ = u.manager.store.Delete(account)
		}
	}
}

func (u *Update) Rollback() {
	if u == nil || u.manager == nil || u.finished {
		return
	}
	if u.active {
		u.manager.mu.Lock()
		for path, previous := range u.previous {
			if previous == "" {
				delete(u.manager.references, path)
			} else {
				u.manager.references[path] = previous
			}
		}
		u.manager.mu.Unlock()
	}
	for _, account := range u.createdAccounts {
		_ = u.manager.store.Delete(account)
	}
	u.finished = true
}

func (m *Manager) referenceInUse(reference string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, current := range m.references {
		if current == reference {
			return true
		}
	}
	return false
}

// WriteConfigAtomically replaces the desktop config with private permissions.
// The temporary file always lives beside the destination so rename remains
// atomic on the target filesystem.
func WriteConfigAtomically(path string, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("desktop config is required")
	}
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return fmt.Errorf("desktop config path must be absolute: %q", path)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode desktop config: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".desktop-config-*")
	if err != nil {
		return fmt.Errorf("create desktop config replacement: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect desktop config replacement: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write desktop config replacement: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync desktop config replacement: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close desktop config replacement: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace desktop config: %w", err)
	}
	return nil
}

type secretSlot struct {
	path string
	get  func() string
	set  func(string)
}

func indexedSecretSlots(cfg *config.Config) (map[string]secretSlot, error) {
	slots := make(map[string]secretSlot)
	err := visitSecretSlots(cfg, func(slot secretSlot) error {
		if _, exists := slots[slot.path]; exists {
			return fmt.Errorf("duplicate desktop credential path %s", slot.path)
		}
		slots[slot.path] = slot
		return nil
	})
	return slots, err
}

func visitSecretSlots(cfg *config.Config, visit func(secretSlot) error) error {
	if cfg == nil || visit == nil {
		return errors.New("desktop config and credential visitor are required")
	}
	slots := []secretSlot{
		stringSlot(PathVision, &cfg.Vision.APIKey),
		stringSlot(PathFOFA, &cfg.FOFA.APIKey),
		stringSlot(PathZoomEye, &cfg.ZoomEye.APIKey),
		stringSlot(PathQuake, &cfg.Quake.APIKey),
		stringSlot(PathShodan, &cfg.Shodan.APIKey),
		stringSlot(PathHitlAuditModel, &cfg.Hitl.AuditModel.APIKey),
		stringSlot(PathKnowledgeEmbedding, &cfg.Knowledge.Embedding.APIKey),
		stringSlot(PathKnowledgeRerank, &cfg.Knowledge.Retrieval.Rerank.APIKey),
	}
	if _, aliasesDefault := defaultAIChannelAlias(cfg); !aliasesDefault {
		slots = append([]secretSlot{stringSlot(PathOpenAI, &cfg.OpenAI.APIKey)}, slots...)
	}
	for _, slot := range slots {
		if err := visit(slot); err != nil {
			return err
		}
	}

	ids := make([]string, 0, len(cfg.AI.Channels))
	for id := range cfg.AI.Channels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seenPaths := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		channelID := id
		path := AIChannelPath(channelID)
		if _, exists := seenPaths[path]; exists {
			return fmt.Errorf("duplicate desktop credential path %s", path)
		}
		seenPaths[path] = struct{}{}
		slot := secretSlot{
			path: path,
			get: func() string {
				return cfg.AI.Channels[channelID].APIKey
			},
			set: func(value string) {
				channel := cfg.AI.Channels[channelID]
				channel.APIKey = value
				cfg.AI.Channels[channelID] = channel
			},
		}
		if err := visit(slot); err != nil {
			return err
		}
	}
	return nil
}

func defaultAIChannelAlias(cfg *config.Config) (string, bool) {
	if cfg == nil || len(cfg.AI.Channels) == 0 {
		return "", false
	}
	defaultID := config.NormalizeAIChannelID(cfg.AI.DefaultChannel)
	for id, channel := range cfg.AI.Channels {
		if config.NormalizeAIChannelID(id) != defaultID {
			continue
		}
		if strings.TrimSpace(channel.APIKey) != strings.TrimSpace(cfg.OpenAI.APIKey) {
			return "", false
		}
		return AIChannelPath(id), true
	}
	return "", false
}

func stringSlot(path string, value *string) secretSlot {
	return secretSlot{
		path: path,
		get:  func() string { return *value },
		set:  func(replacement string) { *value = replacement },
	}
}

func cloneConfig(cfg *config.Config) (*config.Config, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("clone desktop config: %w", err)
	}
	var cloned config.Config
	if err := yaml.Unmarshal(data, &cloned); err != nil {
		return nil, fmt.Errorf("clone desktop config: %w", err)
	}
	return &cloned, nil
}

func cloneReferences(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for path, reference := range source {
		cloned[path] = reference
	}
	return cloned
}

func validAccount(account string) bool {
	if account == "" || len(account) > 128 {
		return false
	}
	for _, character := range account {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func randomAccount() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

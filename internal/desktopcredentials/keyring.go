package desktopcredentials

import "github.com/zalando/go-keyring"

const ServiceName = "com.cyberstrikeai.desktop"

// KeyringStore keeps desktop secrets in the signed-in user's macOS Keychain
// or Windows Credential Manager. The service name is fixed so config files
// only need to contain an opaque account reference.
type KeyringStore struct{}

func (KeyringStore) Get(account string) (string, error) {
	return keyring.Get(ServiceName, account)
}

func (KeyringStore) Set(account, secret string) error {
	return keyring.Set(ServiceName, account, secret)
}

func (KeyringStore) Delete(account string) error {
	return keyring.Delete(ServiceName, account)
}

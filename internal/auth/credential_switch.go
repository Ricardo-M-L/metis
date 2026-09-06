package auth

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Kept as narrow package variables so the cross-store transaction can be
// fault-injected in tests. Production always uses the atomic single-file
// writers below; callers must still hold the shared OAuth store lock.
var (
	credentialSwitchWriteAuthStore  = writeAuthStoreUnlocked
	credentialSwitchWriteOAuthStore = writeOAuthStoreUnlocked
)

// credentialStoreWriteError distinguishes failures before the atomic rename
// from durability/metadata failures reported after the requested bytes are
// already visible at the final path. Cross-store transactions must not roll
// back another store merely because a committed write reported a late error.
type credentialStoreWriteError struct {
	err       error
	committed bool
}

func (e *credentialStoreWriteError) Error() string { return e.err.Error() }
func (e *credentialStoreWriteError) Unwrap() error { return e.err }

func committedCredentialStoreWriteError(err error) error {
	if err == nil {
		return nil
	}
	return &credentialStoreWriteError{err: err, committed: true}
}

func credentialStoreWriteCommitted(err error) bool {
	var writeErr *credentialStoreWriteError
	return errors.As(err, &writeErr) && writeErr.committed
}

// The narrow writer variables are test seams, so a wrapper may successfully
// call the real writer and then return its own error without preserving the
// committed marker. Re-read under the same process/cross-process lock before
// deciding whether rollback is safe.
func writeAuthStoreVerified(path string, desired File) (bool, error) {
	err := credentialSwitchWriteAuthStore(path, desired)
	if err == nil || credentialStoreWriteCommitted(err) {
		return true, err
	}
	actual, found, readErr := readAuthStoreFile(path)
	if readErr == nil && found && reflect.DeepEqual(actual, desired) {
		return true, err
	}
	if readErr != nil {
		return false, errors.Join(err, fmt.Errorf("verify API-key credential store after write error: %w", readErr))
	}
	return false, errors.Join(err, errors.New("API-key credential store did not reach the requested state"))
}

func writeOAuthStoreVerified(path string, desired *oauthStoreFile) (bool, error) {
	err := credentialSwitchWriteOAuthStore(path, desired)
	if err == nil || credentialStoreWriteCommitted(err) {
		return true, err
	}
	actual, found, readErr := loadOAuthStoreFileUnlocked(path)
	if readErr == nil && found && reflect.DeepEqual(actual, desired) {
		return true, err
	}
	if readErr != nil {
		return false, errors.Join(err, fmt.Errorf("verify OAuth credential store after write error: %w", readErr))
	}
	return false, errors.Join(err, errors.New("OAuth credential store did not reach the requested state"))
}

func cloneAuthFile(file File) File {
	cloned := make(File, len(file))
	for provider, entry := range file {
		cloned[provider] = entry
	}
	return cloned
}

func cloneOAuthStore(store *oauthStoreFile) *oauthStoreFile {
	cloned := &oauthStoreFile{
		FormatVersion: store.FormatVersion,
		Credentials:   make(map[string]OAuthCredential, len(store.Credentials)),
	}
	for provider, credential := range store.Credentials {
		cloned.Credentials[provider] = credential
	}
	return cloned
}

func credentialSwitchRollbackError(commitErr, rollbackErr error) error {
	if rollbackErr == nil {
		return commitErr
	}
	return errors.Join(commitErr, fmt.Errorf("restore previous credential state: %w", rollbackErr))
}

// ActivateAPIKey makes key the provider's sole stored login method. Both
// credential files are changed under the same cross-process lock. If removing
// the superseded OAuth record fails, the API-key file is restored before the
// error is returned so a failed switch does not leave both methods installed.
func ActivateAPIKey(provider, key string) error {
	return activateAPIKey(provider, key, nil)
}

// ActivateAPIKeyBound atomically activates an API key together with the exact
// provider endpoint it may be sent to. Custom providers and built-in provider
// IDs routed to non-official endpoints must use this form.
func ActivateAPIKeyBound(provider, key, transport, baseURL string) error {
	binding, err := NormalizeEndpointBinding(provider, transport, baseURL)
	if err != nil {
		return err
	}
	return activateAPIKey(binding.Provider, key, &binding)
}

func activateAPIKey(provider, key string, binding *EndpointBinding) error {
	provider = canonicalOAuthProviderID(provider)
	if provider == "" {
		return errors.New("auth: provider required")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("auth: key required")
	}
	if isMisclassifiedOAuthEntry(provider, key) {
		return errors.New("auth: refusing to activate an OAuth access token as an API key; run `metis login` with the OAuth method")
	}
	layout, err := currentCredentialLayout()
	if err != nil {
		return err
	}
	return withOAuthStoreLock(layout.auth, oauthStoreLockTimeout, func() error {
		apiKeys, err := loadAuthStoreUnlocked(layout)
		if err != nil {
			return err
		}
		oauthStore, err := loadOAuthStoreUnlocked(layout.oauth)
		if err != nil {
			return err
		}
		previousAPIKeys := cloneAuthFile(apiKeys)
		for storedProvider := range apiKeys {
			if storedProvider != provider && canonicalOAuthProviderID(storedProvider) == provider {
				delete(apiKeys, storedProvider)
			}
		}
		apiKeys[provider] = Entry{Type: "api", Key: key, Endpoint: binding}
		apiCommitted, apiWriteErr := writeAuthStoreVerified(layout.auth, apiKeys)
		if !apiCommitted {
			return apiWriteErr
		}
		var delayedErr error
		if apiWriteErr != nil {
			delayedErr = fmt.Errorf("activate API-key credential: %w", apiWriteErr)
		}
		oauthChanged := false
		for storedProvider := range oauthStore.Credentials {
			if canonicalOAuthProviderID(storedProvider) == provider {
				delete(oauthStore.Credentials, storedProvider)
				oauthChanged = true
			}
		}
		if oauthChanged {
			oauthCommitted, oauthWriteErr := writeOAuthStoreVerified(layout.oauth, oauthStore)
			if !oauthCommitted {
				commitErr := errors.Join(delayedErr, fmt.Errorf("remove superseded OAuth credential: %w", oauthWriteErr))
				_, rollbackErr := writeAuthStoreVerified(layout.auth, previousAPIKeys)
				return credentialSwitchRollbackError(commitErr, rollbackErr)
			}
			if oauthWriteErr != nil {
				delayedErr = errors.Join(delayedErr, fmt.Errorf("remove superseded OAuth credential: %w", oauthWriteErr))
			}
		}
		return errors.Join(delayedErr, removeLegacyCredentialFile(layout.legacyAuth), removeLegacyCredentialFile(layout.legacyOAuth))
	})
}

// ActivateOAuth makes credential the provider's sole stored login method.
// If removing a superseded API key fails, the OAuth file is restored before
// the error is returned for the same failure-atomic behavior as ActivateAPIKey.
func ActivateOAuth(provider string, credential OAuthCredential) error {
	return ActivateOAuthContext(context.Background(), provider, credential)
}

// ActivateOAuthContext is the cancellable form used by interactive login.
// Cancellation while waiting for either the in-process or cross-process lock
// is checked again immediately before the first credential-store write.
func ActivateOAuthContext(ctx context.Context, provider string, credential OAuthCredential) error {
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return err
	}
	if err := validateOAuthCredential(credential); err != nil {
		return err
	}
	layout, err := currentCredentialLayout()
	if err != nil {
		return err
	}
	return withOAuthStoreLockContext(ctx, layout.oauth, oauthStoreLockTimeout, func() error {
		oauthStore, err := loadOAuthStoreUnlocked(layout.oauth)
		if err != nil {
			return err
		}
		apiKeys, err := loadAuthStoreUnlocked(layout)
		if err != nil {
			return err
		}
		previousOAuthStore := cloneOAuthStore(oauthStore)
		for storedProvider := range oauthStore.Credentials {
			if storedProvider != provider && canonicalOAuthProviderID(storedProvider) == provider {
				delete(oauthStore.Credentials, storedProvider)
			}
		}
		oauthStore.Credentials[provider] = credential
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		oauthCommitted, oauthWriteErr := writeOAuthStoreVerified(layout.oauth, oauthStore)
		if !oauthCommitted {
			return oauthWriteErr
		}
		var delayedErr error
		if oauthWriteErr != nil {
			delayedErr = fmt.Errorf("activate OAuth credential: %w", oauthWriteErr)
		}
		apiChanged := false
		for storedProvider := range apiKeys {
			if canonicalOAuthProviderID(storedProvider) == provider {
				delete(apiKeys, storedProvider)
				apiChanged = true
			}
		}
		if apiChanged {
			apiCommitted, apiWriteErr := writeAuthStoreVerified(layout.auth, apiKeys)
			if !apiCommitted {
				commitErr := errors.Join(delayedErr, fmt.Errorf("remove superseded API key: %w", apiWriteErr))
				_, rollbackErr := writeOAuthStoreVerified(layout.oauth, previousOAuthStore)
				return credentialSwitchRollbackError(commitErr, rollbackErr)
			}
			if apiWriteErr != nil {
				delayedErr = errors.Join(delayedErr, fmt.Errorf("remove superseded API key: %w", apiWriteErr))
			}
		}
		return errors.Join(delayedErr, removeLegacyCredentialFile(layout.legacyAuth), removeLegacyCredentialFile(layout.legacyOAuth))
	})
}

// RemoveProviderCredentials removes API-key and OAuth records under one
// provider transaction, avoiding a concurrent login being half-removed.
func RemoveProviderCredentials(provider string) error {
	canonicalProvider := canonicalOAuthProviderID(provider)
	if canonicalProvider == "" {
		return errors.New("auth: provider required")
	}
	layout, err := currentCredentialLayout()
	if err != nil {
		return err
	}
	return withOAuthStoreLock(layout.auth, oauthStoreLockTimeout, func() error {
		apiKeys, err := loadAuthStoreUnlocked(layout)
		if err != nil {
			return err
		}
		oauthStore, err := loadOAuthStoreUnlocked(layout.oauth)
		if err != nil {
			return err
		}
		previousAPIKeys := cloneAuthFile(apiKeys)
		apiChanged := false
		for storedProvider := range apiKeys {
			if canonicalOAuthProviderID(storedProvider) == canonicalProvider {
				delete(apiKeys, storedProvider)
				apiChanged = true
			}
		}
		var delayedErr error
		if apiChanged {
			apiCommitted, apiWriteErr := writeAuthStoreVerified(layout.auth, apiKeys)
			if !apiCommitted {
				return apiWriteErr
			}
			if apiWriteErr != nil {
				delayedErr = fmt.Errorf("remove API-key credential: %w", apiWriteErr)
			}
		}
		oauthChanged := false
		for storedProvider := range oauthStore.Credentials {
			if canonicalOAuthProviderID(storedProvider) == canonicalProvider {
				delete(oauthStore.Credentials, storedProvider)
				oauthChanged = true
			}
		}
		if oauthChanged {
			oauthCommitted, oauthWriteErr := writeOAuthStoreVerified(layout.oauth, oauthStore)
			if !oauthCommitted {
				commitErr := errors.Join(delayedErr, fmt.Errorf("remove OAuth credential: %w", oauthWriteErr))
				if apiChanged {
					_, rollbackErr := writeAuthStoreVerified(layout.auth, previousAPIKeys)
					return credentialSwitchRollbackError(commitErr, rollbackErr)
				}
				return commitErr
			}
			if oauthWriteErr != nil {
				delayedErr = errors.Join(delayedErr, fmt.Errorf("remove OAuth credential: %w", oauthWriteErr))
			}
		}
		return errors.Join(delayedErr, removeLegacyCredentialFile(layout.legacyAuth), removeLegacyCredentialFile(layout.legacyOAuth))
	})
}

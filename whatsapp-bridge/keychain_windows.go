//go:build windows

package main

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows Credential Manager storage for the SQLCipher key, via advapi32.
// Same contract as the macOS Keychain / libsecret paths: generate a random
// 256-bit key on first run, return the stored one afterwards. The key never
// touches disk outside the credential store and is never logged.
//
// Implemented with direct CredReadW/CredWriteW syscalls so no new module is
// added (golang.org/x/sys is already in the dependency graph).

var (
	advapi32        = windows.NewLazySystemDLL("advapi32.dll")
	procCredReadW   = advapi32.NewProc("CredReadW")
	procCredWriteW  = advapi32.NewProc("CredWriteW")
	procCredDeleteW = advapi32.NewProc("CredDeleteW")
	procCredFree    = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2 // persists across logons for this user on this machine
)

// winCredential mirrors advapi32's CREDENTIALW struct layout.
type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func credTarget(service, account string) string {
	return service + ":" + account
}

func getOrCreateDBKeyWindows(service, account, dbPath string) (string, error) {
	target := credTarget(service, account)
	return keyStore{
		name:   "Windows Credential Manager",
		remedy: "check Credential Manager access for this user and retry, or set WHATSAPP_DB_KEY to the original key",
		read:   func() (string, keyReadResult, error) { return winClassifiedRead(target) },
		write: func(key string) error {
			// Reachable only after a proven ERROR_NOT_FOUND. CredWriteW has
			// no create-only mode; the classification above is the guard
			// against ever writing over an unread key.
			return winCredWrite(target, account, key)
		},
	}.getOrCreate(dbPath)
}

// winClassifiedRead maps CredReadW outcomes onto the shared three-state
// contract (MYC-3694): ERROR_NOT_FOUND (1168) is the ONLY proven-absent
// signal; every other failure — locked store, revoked access, an empty blob —
// may mean the key still exists, so it must never lead to a mint.
func winClassifiedRead(target string) (string, keyReadResult, error) {
	key, err := winCredRead(target)
	switch {
	case err == nil:
		return key, keyReadOK, nil
	case errors.Is(err, windows.ERROR_NOT_FOUND):
		return "", keyReadNotFound, nil
	default:
		return "", keyReadError, err
	}
}

func winCredRead(target string) (string, error) {
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return "", err
	}
	var pcred *winCredential
	r1, _, e1 := procCredReadW.Call(
		uintptr(unsafe.Pointer(t)),
		uintptr(credTypeGeneric),
		0,
		uintptr(unsafe.Pointer(&pcred)),
	)
	if r1 == 0 {
		return "", fmt.Errorf("CredRead(%s): %w", target, e1)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(pcred)))
	if pcred == nil || pcred.CredentialBlobSize == 0 || pcred.CredentialBlob == nil {
		return "", fmt.Errorf("credential %s has an empty blob", target)
	}
	blob := unsafe.Slice(pcred.CredentialBlob, pcred.CredentialBlobSize)
	return string(blob), nil
}

func winCredWrite(target, user, secret string) error {
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	blob := []byte(secret)
	cred := winCredential{
		Type:               credTypeGeneric,
		TargetName:         t,
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     &blob[0],
		Persist:            credPersistLocalMachine,
		UserName:           u,
	}
	r1, _, e1 := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0)
	runtime.KeepAlive(blob)
	runtime.KeepAlive(&cred)
	if r1 == 0 {
		return fmt.Errorf("CredWrite(%s): %w", target, e1)
	}
	return nil
}

func winCredDelete(target string) error {
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	r1, _, e1 := procCredDeleteW.Call(
		uintptr(unsafe.Pointer(t)),
		uintptr(credTypeGeneric),
		0,
	)
	if r1 == 0 {
		return fmt.Errorf("CredDelete(%s): %w", target, e1)
	}
	return nil
}

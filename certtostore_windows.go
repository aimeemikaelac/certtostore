// +build windows

// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package certtostore

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"github.com/google/logger"
)

const (
	// wincrypt.h constants
	encodingX509ASN         = 1                                               // X509_ASN_ENCODING
	encodingPKCS7           = 65536                                           // PKCS_7_ASN_ENCODING
	certStoreProvSystem     = 10                                              // CERT_STORE_PROV_SYSTEM
	certStoreCurrentUser    = uint32(certStoreCurrentUserID << compareShift)  // CERT_SYSTEM_STORE_CURRENT_USER
	certStoreLocalMachine   = uint32(certStoreLocalMachineID << compareShift) // CERT_SYSTEM_STORE_LOCAL_MACHINE
	certStoreCurrentUserID  = 1                                               // CERT_SYSTEM_STORE_CURRENT_USER_ID
	certStoreLocalMachineID = 2                                               // CERT_SYSTEM_STORE_LOCAL_MACHINE_ID
	infoIssuerFlag          = 4                                               // CERT_INFO_ISSUER_FLAG
	compareNameStrW         = 8                                               // CERT_COMPARE_NAME_STR_A
	compareShift            = 16                                              // CERT_COMPARE_SHIFT
	findIssuerStr           = compareNameStrW<<compareShift | infoIssuerFlag  // CERT_FIND_ISSUER_STR_W
	signatureKeyUsage       = 0x80                                            // CERT_DIGITAL_SIGNATURE_KEY_USAGE
	acquireCached           = 0x1                                             // CRYPT_ACQUIRE_CACHE_FLAG
	acquireSilent           = 0x40                                            // CRYPT_ACQUIRE_SILENT_FLAG
	acquireOnlyNCryptKey    = 0x40000                                         // CRYPT_ACQUIRE_ONLY_NCRYPT_KEY_FLAG
	ncryptKeySpec           = 0xFFFFFFFF                                      // CERT_NCRYPT_KEY_SPEC

	// Legacy CryptoAPI flags
	bCryptPadPKCS1 uintptr = 0x2

	// Magic number for RSA1 public key blobs.
	rsa1Magic = 0x31415352 // "RSA1"
	// https://github.com/dotnet/corefx/blob/master/src/Common/src/Interop/Windows/BCrypt/Interop.Blobs.cs#L92
  ecdsaP256Magic = 0x31534345

	// ncrypt.h constants
	ncryptPersistFlag      = 0x80000000 // NCRYPT_PERSIST_FLAG
	ncryptAllowDecryptFlag = 0x1        // NCRYPT_ALLOW_DECRYPT_FLAG
	ncryptAllowSigningFlag = 0x2        // NCRYPT_ALLOW_SIGNING_FLAG

	// NCryptPadOAEPFlag is used with Decrypt to specify whether to use OAEP.
	NCryptPadOAEPFlag = 0x00000004 // NCRYPT_PAD_OAEP_FLAG

	// key creation flags.
	nCryptMachineKey   = 0x20 // NCRYPT_MACHINE_KEY_FLAG
	nCryptOverwriteKey = 0x80 // NCRYPT_OVERWRITE_KEY_FLAG

	// winerror.h constants
	cryptENotFound = 0x80092004 // CRYPT_E_NOT_FOUND

	// ProviderMSPlatform represents the Microsoft Platform Crypto Provider
	ProviderMSPlatform = "Microsoft Platform Crypto Provider"
	// ProviderMSSoftware represents the Microsoft Software Key Storage Provider
	ProviderMSSoftware = "Microsoft Software Key Storage Provider"
)

var (
	bCryptRSAPublicBlob = wide("RSAPUBLICBLOB")
	bCryptECCPublicBlob = wide("ECCPUBLICBLOB")

	// algIDs maps crypto.Hash values to bcrypt.h constants.
	algIDs = map[crypto.Hash]*uint16{
		crypto.SHA1:   wide("SHA1"),   // BCRYPT_SHA1_ALGORITHM
		crypto.SHA256: wide("SHA256"), // BCRYPT_SHA256_ALGORITHM
		crypto.SHA384: wide("SHA384"), // BCRYPT_SHA384_ALGORITHM
		crypto.SHA512: wide("SHA512"), // BCRYPT_SHA512_ALGORITHM
	}

	// MY, CA and ROOT are well-known system stores that holds certificates.
	// The store that is opened (system or user) depends on the system call used.
	// see https://msdn.microsoft.com/en-us/library/windows/desktop/aa376560(v=vs.85).aspx)
	my   = wide("MY")
	ca   = wide("CA")
	root = wide("ROOT")

	crypt32 = windows.MustLoadDLL("crypt32.dll")
	nCrypt  = windows.MustLoadDLL("ncrypt.dll")

	certDeleteCertificateFromStore  = crypt32.MustFindProc("CertDeleteCertificateFromStore")
	certFindCertificateInStore      = crypt32.MustFindProc("CertFindCertificateInStore")
	certGetIntendedKeyUsage         = crypt32.MustFindProc("CertGetIntendedKeyUsage")
	cryptFindCertificateKeyProvInfo = crypt32.MustFindProc("CryptFindCertificateKeyProvInfo")
	nCryptCreatePersistedKey        = nCrypt.MustFindProc("NCryptCreatePersistedKey")
	nCryptDecrypt                   = nCrypt.MustFindProc("NCryptDecrypt")
	nCryptExportKey                 = nCrypt.MustFindProc("NCryptExportKey")
	nCryptFinalizeKey               = nCrypt.MustFindProc("NCryptFinalizeKey")
	nCryptOpenKey                   = nCrypt.MustFindProc("NCryptOpenKey")
	nCryptOpenStorageProvider       = nCrypt.MustFindProc("NCryptOpenStorageProvider")
	nCryptGetProperty               = nCrypt.MustFindProc("NCryptGetProperty")
	nCryptSetProperty               = nCrypt.MustFindProc("NCryptSetProperty")
	nCryptSignHash                  = nCrypt.MustFindProc("NCryptSignHash")
)

// paddingInfo is the BCRYPT_PKCS1_PADDING_INFO struct in bcrypt.h.
type paddingInfo struct {
	pszAlgID *uint16
}

// wide returns a pointer to a a uint16 representing the equivalent
// to a Windows LPCWSTR.
func wide(s string) *uint16 {
	w := utf16.Encode([]rune(s))
	w = append(w, 0)
	return &w[0]
}

func openProvider(provider string) (uintptr, error) {
	var err error
	var hProv uintptr
	pname := wide(provider)
	// Open the provider, the last parameter is not used
	r, _, err := nCryptOpenStorageProvider.Call(uintptr(unsafe.Pointer(&hProv)), uintptr(unsafe.Pointer(pname)), 0)
	if r == 0 {
		return hProv, nil
	}
	return hProv, fmt.Errorf("NCryptOpenStorageProvider returned %X, %v", r, err)
}

// findCert wraps the CertFindCertificateInStore call. Note that any cert context passed
// into prev will be freed. If no certificate was found, nil will be returned.
func findCert(store windows.Handle, enc, findFlags, findType uint32, para *uint16, prev *windows.CertContext) (*windows.CertContext, error) {
	h, _, err := certFindCertificateInStore.Call(
		uintptr(store),
		uintptr(enc),
		uintptr(findFlags),
		uintptr(findType),
		uintptr(unsafe.Pointer(para)),
		uintptr(unsafe.Pointer(prev)),
	)
	if h == 0 {
		// Actual error, or simply not found?
		if errno, ok := err.(syscall.Errno); ok && errno == cryptENotFound {
			return nil, nil
		}
		return nil, err
	}
	return (*windows.CertContext)(unsafe.Pointer(h)), nil
}

// intendedKeyUsage wraps CertGetIntendedKeyUsage. If there are key usage bytes they will be returned,
// otherwise 0 will be returned. The final parameter (2) represents the size in bytes of &usage.
func intendedKeyUsage(enc uint32, cert *windows.CertContext) (usage uint16) {
	certGetIntendedKeyUsage.Call(uintptr(enc), uintptr(unsafe.Pointer(cert.CertInfo)), uintptr(unsafe.Pointer(&usage)), 2)
	return
}

// WinCertStore is a CertStorage implementation for the Windows Certificate Store.
type WinCertStore struct {
	CStore              windows.Handle
	Prov                uintptr
	ProvName            string
	issuers             []string
	intermediateIssuers []string
	container           string
}

// OpenWinCertStore creates a WinCertStore.
func OpenWinCertStore(provider, container string, issuers, intermediateIssuers []string) (*WinCertStore, error) {
	// Open a handle to the crypto provider we will use for private key operations
	cngProv, err := openProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("unable to open crypto provider or provider not available: %v", err)
	}

	wcs := &WinCertStore{
		Prov:                cngProv,
		ProvName:            provider,
		issuers:             issuers,
		intermediateIssuers: intermediateIssuers,
		container:           container,
	}
	return wcs, nil
}

// Cert returns the current cert associated with this WinCertStore or nil if there isn't one.
func (w *WinCertStore) Cert() (*x509.Certificate, error) {
	return w.cert(w.issuers, my, certStoreLocalMachine)
}

// cert is used by the exported Cert, Intermediate and root functions to lookup certificates.
// store is used to specify which store to perform the lookup in (system or user).
func (w *WinCertStore) cert(issuers []string, searchRoot *uint16, store uint32) (*x509.Certificate, error) {
	// Open a handle to the system cert store
	certStore, err := windows.CertOpenStore(
		certStoreProvSystem,
		0,
		0,
		store,
		uintptr(unsafe.Pointer(searchRoot)))
	if err != nil {
		return nil, fmt.Errorf("store: CertOpenStore returned %v", err)
	}
	defer windows.CertCloseStore(certStore, 0)

	var prev *windows.CertContext
	var cert *x509.Certificate
	for _, issuer := range issuers {
		i, err := windows.UTF16PtrFromString(issuer)
		if err != nil {
			return nil, err
		}

		// pass 0 as the third parameter because it is not used
		// https://msdn.microsoft.com/en-us/library/windows/desktop/aa376064(v=vs.85).aspx
		nc, err := findCert(certStore, encodingX509ASN|encodingPKCS7, 0, findIssuerStr, i, prev)
		if err != nil {
			return nil, fmt.Errorf("finding certificates: %v", err)
		}
		if nc == nil {
			// No certificate found
			continue
		}
		prev = nc
		if (intendedKeyUsage(encodingX509ASN, nc) & signatureKeyUsage) == 0 {
			continue
		}

		// Extract the DER-encoded certificate from the cert context.
		var der []byte
		slice := (*reflect.SliceHeader)(unsafe.Pointer(&der))
		slice.Data = uintptr(unsafe.Pointer(nc.EncodedCert))
		slice.Len = int(nc.Length)
		slice.Cap = int(nc.Length)

		xc, err := x509.ParseCertificate(der)
		if err != nil {
			continue
		}

		cert = xc
		break
	}
	if cert == nil {
		return nil, nil
	}
	return cert, nil
}

// Link will associate the certificate installed in the system store to the user store.
func (w *WinCertStore) Link() error {
	cert, err := w.cert(w.issuers, my, certStoreLocalMachine)
	if err != nil {
		return fmt.Errorf("link: checking for existing machine certificates returned %v", err)
	}

	if cert == nil {
		return nil
	}

	// If the user cert is already there and matches the system cert, return early.
	userCert, err := w.cert(w.issuers, my, certStoreCurrentUser)
	if err != nil {
		return fmt.Errorf("link: checking for existing user certificates returned %v", err)
	}
	if userCert != nil {
		if cert.SerialNumber.Cmp(userCert.SerialNumber) == 0 {
			fmt.Fprintf(os.Stdout, "Certificate %s is already linked to the user certificate store.\n", cert.SerialNumber)
			return nil
		}
	}

	// The user context is missing the cert, or it doesn't match, so proceed with the link.
	certContext, err := windows.CertCreateCertificateContext(
		encodingX509ASN|encodingPKCS7,
		&cert.Raw[0],
		uint32(len(cert.Raw)))
	if err != nil {
		return fmt.Errorf("link: CertCreateCertificateContext returned %v", err)
	}
	defer windows.CertFreeCertificateContext(certContext)

	// Associate the private key we previously generated
	r, _, err := cryptFindCertificateKeyProvInfo.Call(
		uintptr(unsafe.Pointer(certContext)),
		uintptr(uint32(0)),
		0,
	)
	// Windows calls will fill err with a success message, r is what must be checked instead
	if r == 0 {
		fmt.Printf("link: found a matching private key for the certificate, but association failed: %v", err)
	}

	// Open a handle to the user cert store
	userStore, err := windows.CertOpenStore(
		certStoreProvSystem,
		0,
		0,
		certStoreCurrentUser,
		uintptr(unsafe.Pointer(my)))
	if err != nil {
		return fmt.Errorf("link: CertOpenStore for the user store returned %v", err)
	}
	defer windows.CertCloseStore(userStore, 0)

	// Add the cert context to the users certificate store
	if err := windows.CertAddCertificateContextToStore(userStore, certContext, windows.CERT_STORE_ADD_ALWAYS, nil); err != nil {
		return fmt.Errorf("link: CertAddCertificateContextToStore returned %v", err)
	}

	logger.Infof("Successfully linked to existing system certificate with serial %s.", cert.SerialNumber)
	fmt.Fprintf(os.Stdout, "Successfully linked to existing system certificate with serial %s.\n", cert.SerialNumber)
	return nil
}

// Remove removes certificates issued by any of w.issuers from the user and/or system cert stores.
// If it is unable to remove any certificates, it returns an error.
func (w *WinCertStore) Remove(removeSystem bool) error {
	for _, issuer := range w.issuers {
		if err := w.remove(issuer, removeSystem); err != nil {
			return err
		}
	}
	return nil
}

// remove removes a certificate issued by w.issuer from the user and/or system cert stores.
func (w *WinCertStore) remove(issuer string, removeSystem bool) error {
	userStore, err := windows.CertOpenStore(
		certStoreProvSystem,
		0,
		0,
		certStoreCurrentUser,
		uintptr(unsafe.Pointer(my)))
	if err != nil {
		return fmt.Errorf("remove: certopenstore for the user store returned %v", err)
	}
	defer windows.CertCloseStore(userStore, 0)

	userCertContext, err := findCert(
		userStore,
		encodingX509ASN|encodingPKCS7,
		0,
		findIssuerStr,
		wide(issuer),
		nil)
	if err != nil {
		return fmt.Errorf("remove: finding user certificate issued by %s failed: %v", issuer, err)
	}

	if userCertContext != nil {
		if err := removeCert(userCertContext); err != nil {
			return fmt.Errorf("failed to remove user cert: %v", err)
		}
		logger.Info("Cleaned up a user certificate.")
		fmt.Fprintln(os.Stderr, "Cleaned up a user certificate.")
	}

	// if we're only removing the user cert, return early.
	if !removeSystem {
		return nil
	}

	systemStore, err := windows.CertOpenStore(
		certStoreProvSystem,
		0,
		0,
		certStoreLocalMachine,
		uintptr(unsafe.Pointer(my)))
	if err != nil {
		return fmt.Errorf("remove: certopenstore for the system store returned %v", err)
	}
	defer windows.CertCloseStore(systemStore, 0)

	systemCertContext, err := findCert(
		systemStore,
		encodingX509ASN|encodingPKCS7,
		0,
		findIssuerStr,
		wide(issuer),
		nil)
	if err != nil {
		return fmt.Errorf("remove: finding system certificate issued by %s failed: %v", issuer, err)
	}

	if systemCertContext != nil {
		if err := removeCert(systemCertContext); err != nil {
			return fmt.Errorf("failed to remove system cert: %v", err)
		}
		logger.Info("Cleaned up a system certificate.")
		fmt.Fprintln(os.Stderr, "Cleaned up a system certificate.")
	}

	return nil
}

// removeCert wraps CertDeleteCertificateFromStore. If the call succeeds, nil is returned, otherwise
// the extended error is returned.
func removeCert(certContext *windows.CertContext) error {
	r, _, err := certDeleteCertificateFromStore.Call(uintptr(unsafe.Pointer(certContext)))
	if r != 1 {
		return fmt.Errorf("certdeletecertificatefromstore failed with %X: %v", r, err)
	}
	return nil
}

// Intermediate returns the current intermediate cert associated with this
// WinCertStore or nil if there isn't one.
func (w *WinCertStore) Intermediate() (*x509.Certificate, error) {
	//TODO parameterize which cert store to use.
	return w.cert(w.intermediateIssuers, my, certStoreCurrentUser)
}

// Root returns the certificate issued by the specified issuer from the
// root certificate store 'ROOT/Certificates'.
func (w *WinCertStore) Root(issuer []string) (*x509.Certificate, error) {
	return w.cert(issuer, root, certStoreLocalMachine)
}

type Key interface {
	Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error)
	// Decrypt(rand io.Reader, blob []byte, opts crypto.DecrypterOpts) ([]byte, error)
	Public() crypto.PublicKey
	// SetACL(store *WinCertStore, access string, sid string, perm string) error
}

// EcdsaKey and RsaKey implement crypto.Signer and crypto.Decrypter for key based operations.
type EcdsaKey struct {
	handle	  uintptr
	pub			  *ecdsa.PublicKey
	Container	string
}

type RsaKey struct {
	handle	  uintptr
	pub			  *rsa.PublicKey
	Container	string
}

// Public exports a public key to implement crypto.Signer
func (rk *RsaKey) Public() crypto.PublicKey {
	return rk.pub
}

func (ek *EcdsaKey) Public() crypto.PublicKey {
	return ek.pub
}

// Sign returns the signature of a hash to implement crypto.Signer
func (k *RsaKey) Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	hf := opts.HashFunc()
	algID, ok := algIDs[hf]
	if !ok {
		return nil, fmt.Errorf("unsupported hash algorithm %v", hf)
	}

	return rsaSign(k.handle, digest, algID)
}

func (k *EcdsaKey) Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
  hf := opts.HashFunc()
  algID, ok := algIDs[hf]
  if !ok {
		return nil, fmt.Errorf("unsupported hash algorithm %v", hf)
	}

	return ecdsaSign(k.handle, digest, algID)
}

func ecdsaSign(kh uintptr, digest []byte, algID *uint16) ([]byte, error) {
	// padInfo := paddingInfo{pszAlgID: algID}
	var size uint32
	// Obtain the size of the signature
	// r, _, err := nCryptSignHash.Call(
	// 	kh,
	// 	uintptr(unsafe.Pointer(&padInfo)),
	// 	uintptr(unsafe.Pointer(&digest[0])),
	// 	uintptr(len(digest)),
	// 	0,
	// 	0,
	// 	uintptr(unsafe.Pointer(&size)),
	// 	bCryptPadPKCS1)
  r, _, err := nCryptSignHash.Call(
		kh,
		uintptr(0),
		uintptr(unsafe.Pointer(&digest[0])),
		uintptr(len(digest)),
		0,
		0,
		uintptr(unsafe.Pointer(&size)),
		0)
	if r != 0 {
		return nil, fmt.Errorf("NCryptSignHash returned %X during size check: %v", r, err)
	}

	// Obtain the signature data
	sig := make([]byte, size)
	r, _, err = nCryptSignHash.Call(
		kh,
		uintptr(0),
		uintptr(unsafe.Pointer(&digest[0])),
		uintptr(len(digest)),
		uintptr(unsafe.Pointer(&sig[0])),
		uintptr(size),
		uintptr(unsafe.Pointer(&size)),
		0)
	if r != 0 {
		return nil, fmt.Errorf("NCryptSignHash returned %X during signing: %v", r, err)
	}

	return sig[:size], nil
}

func rsaSign(kh uintptr, digest []byte, algID *uint16) ([]byte, error) {
	padInfo := paddingInfo{pszAlgID: algID}
	var size uint32
	// Obtain the size of the signature
	r, _, err := nCryptSignHash.Call(
		kh,
		uintptr(unsafe.Pointer(&padInfo)),
		uintptr(unsafe.Pointer(&digest[0])),
		uintptr(len(digest)),
		0,
		0,
		uintptr(unsafe.Pointer(&size)),
		bCryptPadPKCS1)
	if r != 0 {
		return nil, fmt.Errorf("NCryptSignHash returned %X during size check: %v", r, err)
	}

	// Obtain the signature data
	sig := make([]byte, size)
	r, _, err = nCryptSignHash.Call(
		kh,
		uintptr(unsafe.Pointer(&padInfo)),
		uintptr(unsafe.Pointer(&digest[0])),
		uintptr(len(digest)),
		uintptr(unsafe.Pointer(&sig[0])),
		uintptr(size),
		uintptr(unsafe.Pointer(&size)),
		bCryptPadPKCS1)
	if r != 0 {
		return nil, fmt.Errorf("NCryptSignHash returned %X during signing: %v", r, err)
	}

	return sig[:size], nil
}

// DecrypterOpts implements crypto.DecrypterOpts and contains the
// flags required for the NCryptDecrypt system call.
type DecrypterOpts struct {
	// Hashfunc represents the hashing function that was used during
	// encryption and is mapped to the Microsoft equivalent LPCWSTR.
	Hashfunc crypto.Hash
	// Flags represents the dwFlags parameter for NCryptDecrypt
	Flags uint32
}

// oaepPaddingInfo is the BCRYPT_OAEP_PADDING_INFO struct in bcrypt.h.
// https://msdn.microsoft.com/en-us/library/windows/desktop/aa375526(v=vs.85).aspx
type oaepPaddingInfo struct {
	pszAlgID *uint16 // pszAlgId
	pbLabel  *uint16 // pbLabel
	cbLabel  uint32  // cbLabel
}

// Decrypt returns the decrypted contents of the encrypted blob, and implements
// crypto.Decrypter for Key.
func (k *RsaKey) Decrypt(rand io.Reader, blob []byte, opts crypto.DecrypterOpts) ([]byte, error) {
	decrypterOpts, ok := opts.(DecrypterOpts)
	if !ok {
		return nil, errors.New("opts was not certtostore.DecrypterOpts")
	}

	algID, ok := algIDs[decrypterOpts.Hashfunc]
	if !ok {
		return nil, fmt.Errorf("unsupported hash algorithm %v", decrypterOpts.Hashfunc)
	}

	padding := oaepPaddingInfo{
		pszAlgID: algID,
		pbLabel:  wide(""),
		cbLabel:  0,
	}

	return rsaDecrypt(k.handle, blob, padding, decrypterOpts.Flags)
}

// func (k *EcdsaKey) Decrypt(rand io.Reader, blob []byte, opts crypto.DecrypterOpts) ([]byte, error) {
// 	return nil, fmt.Errorf("ECDSA does not support decryption")
// }

// decrypt wraps the NCryptDecrypt function and returns the decrypted bytes
// that were previously encrypted by NCryptEncrypt or another compatible
// function such as rsa.EncryptOAEP.
// https://msdn.microsoft.com/en-us/library/windows/desktop/aa376249(v=vs.85).aspx
func rsaDecrypt(kh uintptr, blob []byte, padding oaepPaddingInfo, flags uint32) ([]byte, error) {
	var size uint32
	// Obtain the size of the decrypted data
	r, _, err := nCryptDecrypt.Call(
		kh,                                // hKey
		uintptr(unsafe.Pointer(&blob[0])), // pbInput
		uintptr(len(blob)),                // cbInput
		uintptr(unsafe.Pointer(&padding)), // *pPaddingInfo
		0,                                 // pbOutput, must be null on first run
		0,                                 // cbOutput, ignored on first run
		uintptr(unsafe.Pointer(&size)),    // pcbResult
		uintptr(flags))
	if r != 0 {
		return nil, fmt.Errorf("NCryptDecrypt returned %X during size check: %v", r, err)
	}

	// Decrypt the message
	plainText := make([]byte, size)
	r, _, err = nCryptDecrypt.Call(
		kh,                                     // hKey
		uintptr(unsafe.Pointer(&blob[0])),      // pbInput
		uintptr(len(blob)),                     // cbInput
		uintptr(unsafe.Pointer(&padding)),      // *pPaddingInfo
		uintptr(unsafe.Pointer(&plainText[0])), // pbOutput, must be null on first run
		uintptr(size),                          // cbOutput, ignored on first run
		uintptr(unsafe.Pointer(&size)),         // pcbResult
		uintptr(flags))
	if r != 0 {
		return nil, fmt.Errorf("NCryptDecrypt returned %X during decryption: %v", r, err)
	}

	return plainText[:size], nil
}

// SetACL sets permissions for the private key by wrapping the Microsoft
// icacls utility. For CNG keys (even TPM backed keys), access is controlled
// by NTFS ACLs. icacls is used for simple ACL setting versus more complicated
// API calls.
func (k *RsaKey) SetACL(store *WinCertStore, access string, sid string, perm string) error {
	return setAcl(store, access, sid, perm, k.Container)
}

// func (k *EcdsaKey) SetACL(store *WinCertStore, access string, sid string, perm string) error {
// 	return setAcl(store, access, sid, perm, k.Container)
// }

func setAcl(store *WinCertStore, access, sid, perm, loc string) error {
	// loc := k.Container
	logger.Infof("running: icacls.exe %s /%s %s:%s", loc, access, sid, perm)

	// Run icacls as specified, parameter validation prior to this point isn't
	// needed because icacls handles this on its own
	err := exec.Command("icacls.exe", loc, "/"+access, sid+":"+perm).Run()

	// Error 1798 can safely be ignored, because it occurs when trying to set an acl
	// for a non-existend sid, which only happens for certain permissions needed on later
	// versions of Windows, which are not needed on Windows 7.
	if err, ok := err.(*exec.ExitError); ok && strings.Contains(err.Error(), "1798") == false {
		logger.Infof("ignoring error while %sing '%s' access to %s for sid: %v", access, perm, loc, sid)
		return nil
	} else if err != nil {
		return fmt.Errorf("certstorage.SetFileACL is unable to %s %s access on %s to sid %s, %v", access, perm, loc, sid, err)
	}

	return nil
}

// Key opens a handle to an existing private key and returns key.
// Key implements both crypto.Signer and crypto.Decrypter
func (w *WinCertStore) Key() (Key, error) {
	var kh uintptr
	r, _, err := nCryptOpenKey.Call(
		uintptr(w.Prov),
		uintptr(unsafe.Pointer(&kh)),
		uintptr(unsafe.Pointer(wide(w.container))),
		0,
		nCryptMachineKey)
	if r != 0 {
		return nil, fmt.Errorf("NCryptOpenKey for container %s returned %X: %v", w.container, r, err)
	}

	keyAlgType, err := getKeyType(kh)
	if err != nil {
		return nil, fmt.Errorf("Could not determine algorithm type: %v", err)
	}

	// See https://docs.microsoft.com/en-us/windows/win32/seccng/key-storage-property-identifiers for algorithm types
	switch keyAlgType {
	case "RSA":
		uc, pub, err := rsaKeyMetadata(kh, w)
		if err != nil {
			return nil, err
		}

		return &RsaKey{handle: kh, pub: pub, Container: uc}, nil
	case "ECDSA":
		uc, pub, err := ecdsaKeyMetadata(kh, w)
		if err != nil {
			return nil, err
		}
		return &EcdsaKey{handle: kh, pub: pub, Container: uc}, nil
	default:
		return nil, fmt.Errorf("Unsupported key algorithm: %s", keyAlgType)
	}
}

// Generate returns a crypto.Signer representing either a TPM-backed or
// software backed key, depending on support from the host OS
// key size is set to the maximum supported by Microsoft Software Key Storage Provider
func (w *WinCertStore) Generate(keySize int) (crypto.Signer, error) {
	logger.Infof("Provider: %s", w.ProvName)
	// The MPCP only supports a max keywidth of 2048, due to the TPM specification.
	// https://www.microsoft.com/en-us/download/details.aspx?id=52487
	// The Microsoft Software Key Storage Provider supports a max keywidth of 16384.
	if keySize > 16384 {
		return nil, fmt.Errorf("unsupported keysize, got: %d, want: < %d", keySize, 16384)
	}

	var kh uintptr
	var length = uint32(keySize)
	// Pass 0 as the fifth parameter because it is not used (legacy)
	// https://msdn.microsoft.com/en-us/library/windows/desktop/aa376247(v=vs.85).aspx
	r, _, err := nCryptCreatePersistedKey.Call(
		uintptr(w.Prov),
		uintptr(unsafe.Pointer(&kh)),
		uintptr(unsafe.Pointer(wide("RSA"))),
		uintptr(unsafe.Pointer(wide(w.container))),
		0,
		nCryptMachineKey|nCryptOverwriteKey)
	if r != 0 {
		return nil, fmt.Errorf("NCryptCreatePersistedKey returned %X: %v", r, err)
	}

	// Microsoft function calls return actionable return codes in r, err is often filled with text, even when successful
	r, _, err = nCryptSetProperty.Call(
		kh,
		uintptr(unsafe.Pointer(wide("Length"))),
		uintptr(unsafe.Pointer(&length)),
		unsafe.Sizeof(length),
		ncryptPersistFlag)
	if r != 0 {
		return nil, fmt.Errorf("NCryptSetProperty (Length) returned %X: %v", r, err)
	}

	var usage uint32
	usage = ncryptAllowDecryptFlag | ncryptAllowSigningFlag
	r, _, err = nCryptSetProperty.Call(
		kh,
		uintptr(unsafe.Pointer(wide("Key Usage"))),
		uintptr(unsafe.Pointer(&usage)),
		unsafe.Sizeof(usage),
		ncryptPersistFlag)
	if r != 0 {
		return nil, fmt.Errorf("NCryptSetProperty (Key Usage) returned %X: %v", r, err)
	}

	// Set the second parameter to 0 because we require no flags
	// https://msdn.microsoft.com/en-us/library/windows/desktop/aa376265(v=vs.85).aspx
	r, _, err = nCryptFinalizeKey.Call(kh, 0)
	if r != 0 {
		return nil, fmt.Errorf("NCryptFinalizeKey returned %X: %v", r, err)
	}

	keyAlgType, err := getKeyType(kh)
	if err != nil {
		return nil, fmt.Errorf("Could not determine algorithm type: %v", err)
	}

	// See https://docs.microsoft.com/en-us/windows/win32/seccng/key-storage-property-identifiers for algorithm types
	switch keyAlgType {
	case "RSA":
		uc, pub, err := rsaKeyMetadata(kh, w)
		if err != nil {
			return nil, err
		}

		return &RsaKey{handle: kh, pub: pub, Container: uc}, nil
	case "ECDSA":
		return nil, fmt.Errorf("Unsupported key algorithm: %s", keyAlgType)
	default:
		return nil, fmt.Errorf("Unsupported key algorithm: %s", keyAlgType)
	}
}

func getKeyType(kh uintptr) (string, error) {
	var strSize uint32
	r, _, err := nCryptGetProperty.Call(
		kh,
		uintptr(unsafe.Pointer(wide("Algorithm Group"))),
		0,
		0,
		uintptr(unsafe.Pointer(&strSize)),
		0,
		0)
	if r != 0 {
		return "", fmt.Errorf("NCryptGetProperty returned %X during size check, %v", r, err)
	}

	buf := make([]byte, strSize)
	r, _, err = nCryptGetProperty.Call(
		kh,
		uintptr(unsafe.Pointer(wide("Algorithm Group"))),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(strSize),
		uintptr(unsafe.Pointer(&strSize)),
		0,
		0)
	if r != 0 {
		return "", fmt.Errorf("NCryptGetProperty returned %X during export, %v", r, err)
	}

	algGroup := strings.Replace(string(buf), string(0x00), "", -1)
	return algGroup, nil
}

func rsaKeyMetadata(kh uintptr, store *WinCertStore) (string, *rsa.PublicKey, error) {
	// uc is used to populate the container attribute of the private key
	uc, err := container(kh)
	if err != nil {
		return "", nil, err
	}

	// Adjust the key storage location if we have a software backed key
	if store.ProvName == ProviderMSSoftware {
		uc = os.Getenv("ProgramData") + `\Microsoft\Crypto\Keys\` + uc
	}

	pub, err := exportRSA(kh)
	if err != nil {
		return "", nil, fmt.Errorf("failed to export public key: %v", err)
	}

	return uc, pub, nil
}

func ecdsaKeyMetadata(kh uintptr, store *WinCertStore) (string, *ecdsa.PublicKey, error) {
  // uc is used to populate the container attribute of the private key
  uc, err := container(kh)
  if err != nil {
    return "", nil, err
  }

	// Adjust the key storage location if we have a software backed key
	if store.ProvName == ProviderMSSoftware {
		uc = os.Getenv("ProgramData") + `\Microsoft\Crypto\Keys\` + uc
	}

  pub, err := exportEcdsa(kh)
	if err != nil {
		return "", nil, fmt.Errorf("failed to export public key: %v", err)
	}
  return uc, pub, nil
}

func exportEcdsa(kh uintptr) (*ecdsa.PublicKey, error) {
  var size uint32
  r, _, err := nCryptExportKey.Call(
    kh,
    0,
    uintptr(unsafe.Pointer(bCryptECCPublicBlob)),
    0,
    0,
    0,
    uintptr(unsafe.Pointer(&size)),
    0)
  if r != 0 {
    return nil, fmt.Errorf("NCryptExportKey returned %X during size check: %s", r, err)
  }

  buf := make([]byte, size)
  r, _, err = nCryptExportKey.Call(
    kh,
    0,
    uintptr(unsafe.Pointer(bCryptECCPublicBlob)),
    0,
    uintptr(unsafe.Pointer(&buf[0])),
    uintptr(size),
    uintptr(unsafe.Pointer(&size)),
    0)
  if r != 0 {
    return nil, fmt.Errorf("NCryptExportKey returned %X during export: %v", r, err)
  }

  return unmarshalEcdsa(buf, kh)
}

func unmarshalEcdsa(buf []byte, kh uintptr) (*ecdsa.PublicKey, error) {
	// BCRYPT_RSA_BLOB from bcrypt.h
	header := struct {
		Magic uint32
		CBKey uint32
	}{}

	r := bytes.NewReader(buf)
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		return nil, err
	}

	if header.Magic != ecdsaP256Magic {
		return nil, fmt.Errorf("invalid header magic %x", header.Magic)
	}

	x := make([]byte, header.CBKey)
  // 8 bytes is the length of the header, as it
  n, err := r.Read(x)
  if err != nil {
    return nil, fmt.Errorf("Failed to read curve point x: %s", err)
  }
  if n != int(header.CBKey) {
    return nil, fmt.Errorf("Failed to read in %d bytes for the curve point x. Actually read %d bytes", int(header.CBKey), n)
  }

  y := make([]byte, header.CBKey)
	n, err = r.Read(y)
  if err != nil {
    return nil, fmt.Errorf("Failed to read curve point y: %s", err)
  }
  if n != int(header.CBKey) {
    return nil, fmt.Errorf("Failed to read in %d bytes for the curve point y. Actually read %d bytes", int(header.CBKey), n)
  }

	curve, err := getEcdsaCurve(kh)
	if err != nil {
		return nil, fmt.Errorf("Failed to determine elliptic curve for ECDSA key: %v", err)
	}

	pub := &ecdsa.PublicKey{
    Curve: curve,
    X: new(big.Int).SetBytes(x),
    Y: new(big.Int).SetBytes(y),
	}
	return pub, nil
}

func getEcdsaCurve(kh uintptr) (elliptic.Curve, error) {
	var length uint32
	// See https://docs.microsoft.com/en-us/windows/win32/seccng/cng-algorithm-identifiers for algorithm identifiers
	r, _, err := nCryptGetProperty.Call(
		kh,
		uintptr(unsafe.Pointer(wide("Length"))),
		0,
		0,
		uintptr(unsafe.Pointer(&length)),
		0,
		0)
	if r != 0 {
		return nil, fmt.Errorf("NCryptGetProperty returned %X during size check, %v", r, err)
	}

	buf := make([]byte, length)
	r, _, err = nCryptGetProperty.Call(
		kh,
		uintptr(unsafe.Pointer(wide("Length"))),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(length),
		uintptr(unsafe.Pointer(&length)),
		0,
		0)
	if r != 0 {
		return nil, fmt.Errorf("NCryptGetProperty returned %X during export, %v", r, err)
	}

	bits := binary.LittleEndian.Uint32(buf)
	switch bits {
	case 256:
		return elliptic.P256(), nil
	case 384:
		return elliptic.P384(), nil
	case 521:
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("Unsupported ECDSA curve: %v", bits)
	}
}

// container returns the unique container name of a private key
func container(kh uintptr) (string, error) {
	var strSize uint32
	r, _, err := nCryptGetProperty.Call(
		kh,
		uintptr(unsafe.Pointer(wide("Unique Name"))),
		0,
		0,
		uintptr(unsafe.Pointer(&strSize)),
		0,
		0)
	if r != 0 {
		return "", fmt.Errorf("NCryptGetProperty returned %X during size check, %v", r, err)
	}

	buf := make([]byte, strSize)
	r, _, err = nCryptGetProperty.Call(
		kh,
		uintptr(unsafe.Pointer(wide("Unique Name"))),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(strSize),
		uintptr(unsafe.Pointer(&strSize)),
		0,
		0)
	if r != 0 {
		return "", fmt.Errorf("NCryptGetProperty returned %X during export, %v", r, err)
	}

	uc := strings.Replace(string(buf), string(0x00), "", -1)
	return uc, nil
}

func exportRSA(kh uintptr) (*rsa.PublicKey, error) {
	var size uint32
	// When obtaining the size of a public key, most parameters are not required
	r, _, err := nCryptExportKey.Call(
		kh,
		0,
		uintptr(unsafe.Pointer(bCryptRSAPublicBlob)),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&size)),
		0)
	if r != 0 {
		return nil, fmt.Errorf("NCryptExportKey returned %X during size check: %v", r, err)
	}

	// Place the exported key in buf now that we know the size required
	buf := make([]byte, size)
	r, _, err = nCryptExportKey.Call(
		kh,
		0,
		uintptr(unsafe.Pointer(bCryptRSAPublicBlob)),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(size),
		uintptr(unsafe.Pointer(&size)),
		0)
	if r != 0 {
		return nil, fmt.Errorf("NCryptExportKey returned %X during export: %v", r, err)
	}

	return unmarshalRSA(buf)
}

func unmarshalRSA(buf []byte) (*rsa.PublicKey, error) {
	// BCRYPT_RSA_BLOB from bcrypt.h
	header := struct {
		Magic         uint32
		BitLength     uint32
		PublicExpSize uint32
		ModulusSize   uint32
		UnusedPrime1  uint32
		UnusedPrime2  uint32
	}{}

	r := bytes.NewReader(buf)
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		return nil, err
	}

	if header.Magic != rsa1Magic {
		return nil, fmt.Errorf("invalid header magic %x", header.Magic)
	}

	if header.PublicExpSize > 8 {
		return nil, fmt.Errorf("unsupported public exponent size (%d bits)", header.PublicExpSize*8)
	}

	exp := make([]byte, 8)
	if n, err := r.Read(exp[8-header.PublicExpSize:]); n != int(header.PublicExpSize) || err != nil {
		return nil, fmt.Errorf("failed to read public exponent (%d, %v)", n, err)
	}

	mod := make([]byte, header.ModulusSize)
	if n, err := r.Read(mod); n != int(header.ModulusSize) || err != nil {
		return nil, fmt.Errorf("failed to read modulus (%d, %v)", n, err)
	}

	pub := &rsa.PublicKey{
		N: new(big.Int).SetBytes(mod),
		E: int(binary.BigEndian.Uint64(exp)),
	}
	return pub, nil
}

// Store imports certificates into the Windows certificate store
func (w *WinCertStore) Store(cert *x509.Certificate, intermediate *x509.Certificate) error {
	certContext, err := windows.CertCreateCertificateContext(
		encodingX509ASN|encodingPKCS7,
		&cert.Raw[0],
		uint32(len(cert.Raw)))
	if err != nil {
		return fmt.Errorf("store: CertCreateCertificateContext returned %v", err)
	}
	defer windows.CertFreeCertificateContext(certContext)

	// Associate the private key we previously generated
	r, _, err := cryptFindCertificateKeyProvInfo.Call(
		uintptr(unsafe.Pointer(certContext)),
		uintptr(uint32(0)),
		0,
	)
	// Windows calls will fill err with a success message, r is what must be checked instead
	if r == 0 {
		return fmt.Errorf("store: found a matching private key for this certificate, but association failed: %v", err)
	}

	// Open a handle to the system cert store
	systemStore, err := windows.CertOpenStore(
		certStoreProvSystem,
		0,
		0,
		certStoreLocalMachine,
		uintptr(unsafe.Pointer(my)))
	if err != nil {
		return fmt.Errorf("store: CertOpenStore for the system store returned %v", err)
	}
	defer windows.CertCloseStore(systemStore, 0)

	// Add the cert context to the system certificate store
	if err := windows.CertAddCertificateContextToStore(systemStore, certContext, windows.CERT_STORE_ADD_ALWAYS, nil); err != nil {
		return fmt.Errorf("store: CertAddCertificateContextToStore returned %v", err)
	}

	// Prep the intermediate cert context
	intContext, err := windows.CertCreateCertificateContext(
		encodingX509ASN|encodingPKCS7,
		&intermediate.Raw[0],
		uint32(len(intermediate.Raw)))
	if err != nil {
		return fmt.Errorf("store: CertCreateCertificateContext returned %v", err)
	}
	defer windows.CertFreeCertificateContext(intContext)

	// Open a handle to the intermediate cert store
	caStore, err := windows.CertOpenStore(
		certStoreProvSystem,
		0,
		0,
		certStoreLocalMachine,
		uintptr(unsafe.Pointer(ca)))
	if err != nil {
		return fmt.Errorf("store: CertOpenStore for the intermediate store returned %v", err)
	}
	defer windows.CertCloseStore(caStore, 0)

	// Add the intermediate cert context to the store
	if err := windows.CertAddCertificateContextToStore(caStore, intContext, windows.CERT_STORE_ADD_ALWAYS, nil); err != nil {
		return fmt.Errorf("store: CertAddCertificateContextToStore returned %v", err)
	}

	return nil
}

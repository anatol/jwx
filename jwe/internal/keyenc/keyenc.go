package keyenc

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"hash"
	"math/big"

	"github.com/lestrrat-go/jwx/internal/concatkdf"
	"github.com/lestrrat-go/jwx/internal/ecutil"
	"github.com/lestrrat-go/jwx/jwa"
	contentcipher "github.com/lestrrat-go/jwx/jwe/internal/cipher"
	"github.com/lestrrat-go/jwx/jwe/internal/keygen"
	"github.com/lestrrat-go/pdebug"
	"github.com/pkg/errors"
)

// NewAESCGM creates a key-wrap encrypter using AES-CGM.
// Although the name suggests otherwise, this does the decryption as well.
func NewAESCGM(alg jwa.KeyEncryptionAlgorithm, sharedkey []byte) (*AESCGM, error) {
	return &AESCGM{
		alg:       alg,
		sharedkey: sharedkey,
	}, nil
}

// Algorithm returns the key encryption algorithm being used
func (kw *AESCGM) Algorithm() jwa.KeyEncryptionAlgorithm {
	return kw.alg
}

// KeyID returns the key ID associated with this encrypter
func (kw *AESCGM) KeyID() string {
	return kw.keyID
}

// Decrypt decrypts the encrypted key using AES-CGM key unwrap
func (kw *AESCGM) Decrypt(enckey []byte) ([]byte, error) {
	block, err := aes.NewCipher(kw.sharedkey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cipher from shared key")
	}

	cek, err := Unwrap(block, enckey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unwrap data")
	}
	return cek, nil
}

// KeyEncrypt encrypts the given content encryption key
func (kw *AESCGM) Encrypt(cek []byte) (keygen.ByteSource, error) {
	block, err := aes.NewCipher(kw.sharedkey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cipher from shared key")
	}
	encrypted, err := Wrap(block, cek)
	if err != nil {
		return nil, errors.Wrap(err, `keywrap: failed to wrap key`)
	}
	return keygen.ByteKey(encrypted), nil
}

// NewECDHESEncrypt creates a new key encrypter based on ECDH-ES
func NewECDHESEncrypt(alg jwa.KeyEncryptionAlgorithm, key *ecdsa.PublicKey) (*ECDHESEncrypt, error) {
	generator, err := keygen.NewEcdhes(alg, key)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create key generator")
	}
	return &ECDHESEncrypt{
		algorithm: alg,
		generator: generator,
	}, nil
}

// Algorithm returns the key encryption algorithm being used
func (kw ECDHESEncrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return kw.algorithm
}

// KeyID returns the key ID associated with this encrypter
func (kw ECDHESEncrypt) KeyID() string {
	return kw.keyID
}

// KeyEncrypt encrypts the content encryption key using ECDH-ES
func (kw ECDHESEncrypt) Encrypt(cek []byte) (keygen.ByteSource, error) {
	kg, err := kw.generator.Generate()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create key generator")
	}

	bwpk, ok := kg.(keygen.ByteWithECPrivateKey)
	if !ok {
		return nil, errors.New("key generator generated invalid key (expected ByteWithECPrivateKey)")
	}

	block, err := aes.NewCipher(bwpk.Bytes())
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate cipher from generated key")
	}

	jek, err := Wrap(block, cek)
	if err != nil {
		return nil, errors.Wrap(err, "failed to wrap data")
	}

	bwpk.ByteKey = keygen.ByteKey(jek)

	return bwpk, nil
}

// NewECDHESDecrypt creates a new key decrypter using ECDH-ES
func NewECDHESDecrypt(keyalg jwa.KeyEncryptionAlgorithm, contentalg jwa.ContentEncryptionAlgorithm, pubkey *ecdsa.PublicKey, apu, apv []byte, privkey *ecdsa.PrivateKey) *ECDHESDecrypt {
	return &ECDHESDecrypt{
		keyalg:     keyalg,
		contentalg: contentalg,
		apu:        apu,
		apv:        apv,
		privkey:    privkey,
		pubkey:     pubkey,
	}
}

// Algorithm returns the key encryption algorithm being used
func (kw ECDHESDecrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return kw.keyalg
}

func DeriveECDHES(alg, apu, apv []byte, privkey *ecdsa.PrivateKey, pubkey *ecdsa.PublicKey, keysize uint32) ([]byte, error) {
	if pdebug.Enabled {
		g := pdebug.Marker("DeriveECDHES (keysize = %d)", keysize)
		defer g.End()
	}

	pubinfo := make([]byte, 4)
	binary.BigEndian.PutUint32(pubinfo, keysize*8)

	if !privkey.PublicKey.Curve.IsOnCurve(pubkey.X, pubkey.Y) {
		return nil, errors.New(`public key must be on the same curve as private key`)
	}

	z, _ := privkey.PublicKey.Curve.ScalarMult(pubkey.X, pubkey.Y, privkey.D.Bytes())
	zBytes := ecutil.AllocECPointBuffer(z, privkey.Curve)
	defer ecutil.ReleaseECPointBuffer(zBytes)

	kdf := concatkdf.New(crypto.SHA256, alg, zBytes, apu, apv, pubinfo, []byte{})
	key := make([]byte, keysize)
	if _, err := kdf.Read(key); err != nil {
		return nil, errors.Wrap(err, "failed to read kdf")
	}

	return key, nil
}

// Decrypt decrypts the encrypted key using ECDH-ES
func (kw ECDHESDecrypt) Decrypt(enckey []byte) ([]byte, error) {
	if pdebug.Enabled {
		g := pdebug.Marker("keyenc.ECDHESDecrypt.Decrypt")
		defer g.End()
	}

	var algBytes []byte
	var keysize uint32

	// Use keyalg except for when jwa.ECDH_ES
	algBytes = []byte(kw.keyalg.String())

	switch kw.keyalg {
	case jwa.ECDH_ES:
		// Create a content cipher from the content encryption algorithm
		c, err := contentcipher.NewAES(kw.contentalg)
		if err != nil {
			return nil, errors.Wrapf(err, `failed to create content cipher for %s`, kw.contentalg)
		}
		if pdebug.Enabled {
			pdebug.Printf("Using keysize (%d) from content cipher %a", c.KeySize(), kw.contentalg)
		}

		keysize = uint32(c.KeySize())
		algBytes = []byte(kw.contentalg.String())
	case jwa.ECDH_ES_A128KW:
		keysize = 16
	case jwa.ECDH_ES_A192KW:
		keysize = 24
	case jwa.ECDH_ES_A256KW:
		keysize = 32
	default:
		return nil, errors.Errorf("invalid ECDH-ES key wrap algorithm (%s)", kw.keyalg)
	}

	key, err := DeriveECDHES(algBytes, kw.apu, kw.apv, kw.privkey, kw.pubkey, keysize)
	if err != nil {
		return nil, errors.Wrap(err, `failed to derive ECDHES encryption key`)
	}

	// ECDH-ES does not wrap keys
	if kw.keyalg == jwa.ECDH_ES {
		return key, nil
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cipher for ECDH-ES key wrap")
	}

	return Unwrap(block, enckey)
}

// NewECMRDecrypt creates a new key decrypter using ECMR
func NewECMRDecrypt(keyalg jwa.KeyEncryptionAlgorithm, contentalg jwa.ContentEncryptionAlgorithm, pubkey *ecdsa.PublicKey, apu, apv []byte, exchFn ECMRExchangeFunc) *ECMRDecrypt {
	return &ECMRDecrypt{
		keyalg:     keyalg,
		contentalg: contentalg,
		apu:        apu,
		apv:        apv,
		exchFn:     exchFn,
		pubkey:     pubkey,
	}
}

// Algorithm returns the key encryption algorithm being used
func (kw ECMRDecrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return kw.keyalg
}

func DeriveECMR(alg, apu, apv []byte, exchFn ECMRExchangeFunc, pubkey *ecdsa.PublicKey, keysize uint32) ([]byte, error) {
	if pdebug.Enabled {
		g := pdebug.Marker("DeriveECMR (keysize = %d)", keysize)
		defer g.End()
	}

	pubinfo := make([]byte, 4)
	binary.BigEndian.PutUint32(pubinfo, keysize*8)

	ecCurve := pubkey.Curve // curve used for the key exchange

	tempKey, err := ecdsa.GenerateKey(ecCurve, rand.Reader)
	if err != nil {
		return nil, err
	}

	x, y := ecCurve.Add(tempKey.X, tempKey.Y, pubkey.X, pubkey.Y)

	xfrKey := ecdsa.PublicKey{Curve: ecCurve, X: x, Y: y}

	respKey, srvKey, err := exchFn(&xfrKey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to exchange public key")
	}

	if respKey.Curve != ecCurve {
		return nil, errors.Errorf("expect EC curve type %v, got %v", ecCurve, respKey.Curve)
	}

	if !ecCurve.IsOnCurve(srvKey.X, srvKey.Y) {
		return nil, errors.Errorf("server key is not on the curve %v", ecCurve)
	}

	x, y = ecCurve.ScalarMult(srvKey.X, srvKey.Y, tempKey.D.Bytes())

	// resp - tmp
	z, _ := ecCurve.Add(respKey.X, respKey.Y, x, new(big.Int).Neg(y))
	zBytes := ecutil.AllocECPointBuffer(z, ecCurve)
	defer ecutil.ReleaseECPointBuffer(zBytes)

	kdf := concatkdf.New(crypto.SHA256, alg, zBytes, apu, apv, pubinfo, []byte{})
	key := make([]byte, keysize)
	if _, err := kdf.Read(key); err != nil {
		return nil, errors.Wrap(err, "failed to read kdf")
	}

	return key, nil
}

// Decrypt decrypts the encrypted key using ECMR
func (kw ECMRDecrypt) Decrypt(enckey []byte) ([]byte, error) {
	if pdebug.Enabled {
		g := pdebug.Marker("keyenc.ECMRDecrypt.Decrypt")
		defer g.End()
	}

	var algBytes []byte
	var keysize uint32

	// Use keyalg except for when jwa.ECMR
	algBytes = []byte(kw.keyalg.String())

	switch kw.keyalg {
	case jwa.ECMR:
		// Create a content cipher from the content encryption algorithm
		c, err := contentcipher.NewAES(kw.contentalg)
		if err != nil {
			return nil, errors.Wrapf(err, `failed to create content cipher for %s`, kw.contentalg)
		}
		if pdebug.Enabled {
			pdebug.Printf("Using keysize (%d) from content cipher %a", c.KeySize(), kw.contentalg)
		}

		keysize = uint32(c.KeySize())
		algBytes = []byte(kw.contentalg.String())
	default:
		return nil, errors.Errorf("invalid ECMR key wrap algorithm (%s)", kw.keyalg)
	}

	key, err := DeriveECMR(algBytes, kw.apu, kw.apv, kw.exchFn, kw.pubkey, keysize)
	if err != nil {
		return nil, errors.Wrap(err, `failed to derive ECMR encryption key`)
	}

	// ECMR does not wrap keys
	if kw.keyalg == jwa.ECMR {
		return key, nil
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cipher for ECMR key wrap")
	}

	return Unwrap(block, enckey)
}

// NewRSAOAEPEncrypt creates a new key encrypter using RSA OAEP
func NewRSAOAEPEncrypt(alg jwa.KeyEncryptionAlgorithm, pubkey *rsa.PublicKey) (*RSAOAEPEncrypt, error) {
	switch alg {
	case jwa.RSA_OAEP, jwa.RSA_OAEP_256:
	default:
		return nil, errors.Errorf("invalid RSA OAEP encrypt algorithm (%s)", alg)
	}
	return &RSAOAEPEncrypt{
		alg:    alg,
		pubkey: pubkey,
	}, nil
}

// NewRSAPKCSEncrypt creates a new key encrypter using PKCS1v15
func NewRSAPKCSEncrypt(alg jwa.KeyEncryptionAlgorithm, pubkey *rsa.PublicKey) (*RSAPKCSEncrypt, error) {
	switch alg {
	case jwa.RSA1_5:
	default:
		return nil, errors.Errorf("invalid RSA PKCS encrypt algorithm (%s)", alg)
	}

	return &RSAPKCSEncrypt{
		alg:    alg,
		pubkey: pubkey,
	}, nil
}

// Algorithm returns the key encryption algorithm being used
func (e RSAPKCSEncrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return e.alg
}

// KeyID returns the key ID associated with this encrypter
func (e RSAPKCSEncrypt) KeyID() string {
	return e.keyID
}

// Algorithm returns the key encryption algorithm being used
func (e RSAOAEPEncrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return e.alg
}

// KeyID returns the key ID associated with this encrypter
func (e RSAOAEPEncrypt) KeyID() string {
	return e.keyID
}

// KeyEncrypt encrypts the content encryption key using RSA PKCS1v15
func (e RSAPKCSEncrypt) Encrypt(cek []byte) (keygen.ByteSource, error) {
	if e.alg != jwa.RSA1_5 {
		return nil, errors.Errorf("invalid RSA PKCS encrypt algorithm (%s)", e.alg)
	}
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, e.pubkey, cek)
	if err != nil {
		return nil, errors.Wrap(err, "failed to encrypt using PKCS1v15")
	}
	return keygen.ByteKey(encrypted), nil
}

// KeyEncrypt encrypts the content encryption key using RSA OAEP
func (e RSAOAEPEncrypt) Encrypt(cek []byte) (keygen.ByteSource, error) {
	var hash hash.Hash
	switch e.alg {
	case jwa.RSA_OAEP:
		hash = sha1.New()
	case jwa.RSA_OAEP_256:
		hash = sha256.New()
	default:
		return nil, errors.New("failed to generate key encrypter for RSA-OAEP: RSA_OAEP/RSA_OAEP_256 required")
	}
	encrypted, err := rsa.EncryptOAEP(hash, rand.Reader, e.pubkey, cek, []byte{})
	if err != nil {
		return nil, errors.Wrap(err, `failed to OAEP encrypt`)
	}
	return keygen.ByteKey(encrypted), nil
}

// NewRSAPKCS15Decrypt creates a new decrypter using RSA PKCS1v15
func NewRSAPKCS15Decrypt(alg jwa.KeyEncryptionAlgorithm, privkey *rsa.PrivateKey, keysize int) *RSAPKCS15Decrypt {
	generator := keygen.NewRandom(keysize * 2)
	return &RSAPKCS15Decrypt{
		alg:       alg,
		privkey:   privkey,
		generator: generator,
	}
}

// Algorithm returns the key encryption algorithm being used
func (d RSAPKCS15Decrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return d.alg
}

// Decrypt decryptes the encrypted key using RSA PKCS1v1.5
func (d RSAPKCS15Decrypt) Decrypt(enckey []byte) ([]byte, error) {
	if pdebug.Enabled {
		pdebug.Printf("START PKCS.Decrypt")
	}
	// Hey, these notes and workarounds were stolen from go-jose
	defer func() {
		// DecryptPKCS1v15SessionKey sometimes panics on an invalid payload
		// because of an index out of bounds error, which we want to ignore.
		// This has been fixed in Go 1.3.1 (released 2014/08/13), the recover()
		// only exists for preventing crashes with unpatched versions.
		// See: https://groups.google.com/forum/#!topic/golang-dev/7ihX6Y6kx9k
		// See: https://code.google.com/p/go/source/detail?r=58ee390ff31602edb66af41ed10901ec95904d33
		_ = recover()
	}()

	// Perform some input validation.
	expectedlen := d.privkey.PublicKey.N.BitLen() / 8
	if expectedlen != len(enckey) {
		// Input size is incorrect, the encrypted payload should always match
		// the size of the public modulus (e.g. using a 2048 bit key will
		// produce 256 bytes of output). Reject this since it's invalid input.
		return nil, fmt.Errorf(
			"input size for key decrypt is incorrect (expected %d, got %d)",
			expectedlen,
			len(enckey),
		)
	}

	var err error

	bk, err := d.generator.Generate()
	if err != nil {
		return nil, errors.New("failed to generate key")
	}
	cek := bk.Bytes()

	// When decrypting an RSA-PKCS1v1.5 payload, we must take precautions to
	// prevent chosen-ciphertext attacks as described in RFC 3218, "Preventing
	// the Million Message Attack on Cryptographic Message Syntax". We are
	// therefore deliberately ignoring errors here.
	err = rsa.DecryptPKCS1v15SessionKey(rand.Reader, d.privkey, enckey, cek)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decrypt via PKCS1v15")
	}

	return cek, nil
}

// NewRSAOAEPDecrypt creates a new key decrypter using RSA OAEP
func NewRSAOAEPDecrypt(alg jwa.KeyEncryptionAlgorithm, privkey *rsa.PrivateKey) (*RSAOAEPDecrypt, error) {
	switch alg {
	case jwa.RSA_OAEP, jwa.RSA_OAEP_256:
	default:
		return nil, errors.Errorf("invalid RSA OAEP decrypt algorithm (%s)", alg)
	}

	return &RSAOAEPDecrypt{
		alg:     alg,
		privkey: privkey,
	}, nil
}

// Algorithm returns the key encryption algorithm being used
func (d RSAOAEPDecrypt) Algorithm() jwa.KeyEncryptionAlgorithm {
	return d.alg
}

// Decrypt decryptes the encrypted key using RSA OAEP
func (d RSAOAEPDecrypt) Decrypt(enckey []byte) ([]byte, error) {
	if pdebug.Enabled {
		pdebug.Printf("START OAEP.Decrypt")
	}
	var hash hash.Hash
	switch d.alg {
	case jwa.RSA_OAEP:
		hash = sha1.New()
	case jwa.RSA_OAEP_256:
		hash = sha256.New()
	default:
		return nil, errors.New("failed to generate key encrypter for RSA-OAEP: RSA_OAEP/RSA_OAEP_256 required")
	}
	return rsa.DecryptOAEP(hash, rand.Reader, d.privkey, enckey, []byte{})
}

// Decrypt for DirectDecrypt does not do anything other than
// return a copy of the embedded key
func (d DirectDecrypt) Decrypt() ([]byte, error) {
	cek := make([]byte, len(d.Key))
	copy(cek, d.Key)
	return cek, nil
}

var keywrapDefaultIV = []byte{0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6}

const keywrapChunkLen = 8

func Wrap(kek cipher.Block, cek []byte) ([]byte, error) {
	if len(cek)%8 != 0 {
		return nil, errors.New(`keywrap input must be 8 byte blocks`)
	}

	n := len(cek) / keywrapChunkLen
	r := make([][]byte, n)

	for i := 0; i < n; i++ {
		r[i] = make([]byte, keywrapChunkLen)
		copy(r[i], cek[i*keywrapChunkLen:])
	}

	buffer := make([]byte, keywrapChunkLen*2)
	tBytes := make([]byte, keywrapChunkLen)
	copy(buffer, keywrapDefaultIV)

	for t := 0; t < 6*n; t++ {
		copy(buffer[keywrapChunkLen:], r[t%n])

		kek.Encrypt(buffer, buffer)

		binary.BigEndian.PutUint64(tBytes, uint64(t+1))

		for i := 0; i < keywrapChunkLen; i++ {
			buffer[i] = buffer[i] ^ tBytes[i]
		}
		copy(r[t%n], buffer[keywrapChunkLen:])
	}

	out := make([]byte, (n+1)*keywrapChunkLen)
	copy(out, buffer[:keywrapChunkLen])
	for i := range r {
		copy(out[(i+1)*8:], r[i])
	}

	return out, nil
}

func Unwrap(block cipher.Block, ciphertxt []byte) ([]byte, error) {
	if pdebug.Enabled {
		g := pdebug.Marker("keyenc.Unwrap")
		defer g.End()
	}

	if len(ciphertxt)%keywrapChunkLen != 0 {
		return nil, errors.Errorf(`keyunwrap input must be %d byte blocks`, keywrapChunkLen)
	}

	n := (len(ciphertxt) / keywrapChunkLen) - 1
	r := make([][]byte, n)

	for i := range r {
		r[i] = make([]byte, keywrapChunkLen)
		copy(r[i], ciphertxt[(i+1)*keywrapChunkLen:])
	}

	buffer := make([]byte, keywrapChunkLen*2)
	tBytes := make([]byte, keywrapChunkLen)
	copy(buffer[:keywrapChunkLen], ciphertxt[:keywrapChunkLen])

	for t := 6*n - 1; t >= 0; t-- {
		binary.BigEndian.PutUint64(tBytes, uint64(t+1))

		for i := 0; i < keywrapChunkLen; i++ {
			buffer[i] = buffer[i] ^ tBytes[i]
		}
		copy(buffer[keywrapChunkLen:], r[t%n])

		block.Decrypt(buffer, buffer)

		copy(r[t%n], buffer[keywrapChunkLen:])
	}

	if subtle.ConstantTimeCompare(buffer[:keywrapChunkLen], keywrapDefaultIV) == 0 {
		if pdebug.Enabled {
			pdebug.Printf("buffer prefix does not match default iv")
			pdebug.Printf("prefix  = %x", buffer[:keywrapChunkLen])
			pdebug.Printf("default = %x", keywrapDefaultIV)
		}
		return nil, errors.New("key unwrap: failed to unwrap key")
	}

	out := make([]byte, n*keywrapChunkLen)
	for i := range r {
		copy(out[i*keywrapChunkLen:], r[i])
	}

	return out, nil
}

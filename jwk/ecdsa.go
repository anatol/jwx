package jwk

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"fmt"
	"math/big"

	"github.com/lestrrat-go/jwx/internal/base64"
	"github.com/lestrrat-go/jwx/jwa"
	"github.com/pkg/errors"
)

func NewECDSAPublicKey() ECDSAPublicKey {
	return newECDSAPublicKey()
}

func newECDSAPublicKey() *ecdsaPublicKey {
	return &ecdsaPublicKey{
		privateParams: make(map[string]interface{}),
	}
}

func NewECDSAPrivateKey() ECDSAPrivateKey {
	return newECDSAPrivateKey()
}

func newECDSAPrivateKey() *ecdsaPrivateKey {
	return &ecdsaPrivateKey{
		privateParams: make(map[string]interface{}),
	}
}

func (k *ecdsaPublicKey) FromRaw(rawKey *ecdsa.PublicKey) error {
	k.x = rawKey.X.Bytes()
	k.y = rawKey.Y.Bytes()
	switch rawKey.Curve {
	case elliptic.P256():
		if err := k.Set(ECDSACrvKey, jwa.P256); err != nil {
			return errors.Wrap(err, `failed to set header`)
		}
	case elliptic.P384():
		if err := k.Set(ECDSACrvKey, jwa.P384); err != nil {
			return errors.Wrap(err, `failed to set header`)
		}
	case elliptic.P521():
		if err := k.Set(ECDSACrvKey, jwa.P521); err != nil {
			return errors.Wrap(err, `failed to set header`)
		}
	default:
		return errors.Errorf(`invalid elliptic curve %s`, rawKey.Curve)
	}

	return nil
}

func (k *ecdsaPrivateKey) FromRaw(rawKey *ecdsa.PrivateKey) error {
	k.x = rawKey.X.Bytes()
	k.y = rawKey.Y.Bytes()
	switch rawKey.Curve {
	case elliptic.P256():
		if err := k.Set(ECDSACrvKey, jwa.P256); err != nil {
			return errors.Wrap(err, "failed to write header")
		}
	case elliptic.P384():
		if err := k.Set(ECDSACrvKey, jwa.P384); err != nil {
			return errors.Wrap(err, "failed to write header")
		}
	case elliptic.P521():
		if err := k.Set(ECDSACrvKey, jwa.P521); err != nil {
			return errors.Wrap(err, "failed to write header")
		}
	default:
		return errors.Errorf(`invalid elliptic curve %s`, rawKey.Curve)
	}

	k.d = rawKey.D.Bytes()

	return nil
}

func buildECDSAPublicKey(alg jwa.EllipticCurveAlgorithm, xbuf, ybuf []byte) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch alg {
	case jwa.P256:
		curve = elliptic.P256()
	case jwa.P384:
		curve = elliptic.P384()
	case jwa.P521:
		curve = elliptic.P521()
	default:
		return nil, errors.Errorf(`invalid curve algorithm %s`, alg)
	}

	var x, y big.Int
	x.SetBytes(xbuf)
	y.SetBytes(ybuf)

	return &ecdsa.PublicKey{Curve: curve, X: &x, Y: &y}, nil
}

// Raw returns the EC-DSA public key represented by this JWK
func (k *ecdsaPublicKey) Raw(v interface{}) error {
	pubk, err := buildECDSAPublicKey(k.Crv(), k.x, k.y)
	if err != nil {
		return errors.Wrap(err, `failed to build public key`)
	}

	return assignRawResult(v, pubk)
}

func (k *ecdsaPrivateKey) Raw(v interface{}) error {
	pubk, err := buildECDSAPublicKey(k.Crv(), k.x, k.y)
	if err != nil {
		return errors.Wrap(err, `failed to build public key`)
	}

	var key ecdsa.PrivateKey
	var d big.Int
	d.SetBytes(k.d)
	key.D = &d
	key.PublicKey = *pubk

	return assignRawResult(v, &key)
}

func (k *ecdsaPrivateKey) PublicKey() (ECDSAPublicKey, error) {
	var privk ecdsa.PrivateKey
	if err := k.Raw(&privk); err != nil {
		return nil, errors.Wrap(err, `failed to materialize ECDSA private key`)
	}

	newKey := NewECDSAPublicKey()
	if err := newKey.FromRaw(&privk.PublicKey); err != nil {
		return nil, errors.Wrap(err, `failed to initialize ECDSAPublicKey`)
	}
	return newKey, nil
}

func ecdsaThumbprint(hash crypto.Hash, crv, x, y string) []byte {
	h := hash.New()
	fmt.Fprint(h, `{"crv":"`)
	fmt.Fprint(h, crv)
	fmt.Fprint(h, `","kty":"EC","x":"`)
	fmt.Fprint(h, x)
	fmt.Fprint(h, `","y":"`)
	fmt.Fprint(h, y)
	fmt.Fprint(h, `"}`)
	return h.Sum(nil)
}

// Thumbprint returns the JWK thumbprint using the indicated
// hashing algorithm, according to RFC 7638
func (k ecdsaPublicKey) Thumbprint(hash crypto.Hash) ([]byte, error) {
	var key ecdsa.PublicKey
	if err := k.Raw(&key); err != nil {
		return nil, errors.Wrap(err, `failed to materialize ecdsa.PublicKey for thumbprint generation`)
	}

	curveSize := curveSize(key.Curve)

	return ecdsaThumbprint(
		hash,
		key.Curve.Params().Name,
		base64.EncodeToString(newFixedSizeBuffer(key.X.Bytes(), curveSize)),
		base64.EncodeToString(newFixedSizeBuffer(key.Y.Bytes(), curveSize)),
	), nil
}

// Thumbprint returns the JWK thumbprint using the indicated
// hashing algorithm, according to RFC 7638
func (k ecdsaPrivateKey) Thumbprint(hash crypto.Hash) ([]byte, error) {
	var key ecdsa.PrivateKey
	if err := k.Raw(&key); err != nil {
		return nil, errors.Wrap(err, `failed to materialize ecdsa.PrivateKey for thumbprint generation`)
	}

	curveSize := curveSize(key.Curve)

	return ecdsaThumbprint(
		hash,
		key.Curve.Params().Name,
		base64.EncodeToString(newFixedSizeBuffer(key.X.Bytes(), curveSize)),
		base64.EncodeToString(newFixedSizeBuffer(key.Y.Bytes(), curveSize)),
	), nil
}

// Get size of curve in bytes
func curveSize(crv elliptic.Curve) int {
	bits := crv.Params().BitSize

	div := bits / 8
	mod := bits % 8

	if mod == 0 {
		return div
	}

	return div + 1
}

func newFixedSizeBuffer(data []byte, length int) []byte {
	if len(data) > length {
		panic("square/go-jose: invalid call to newFixedSizeBuffer (len(data) > length)")
	}
	pad := make([]byte, length-len(data))
	return append(pad, data...)
}

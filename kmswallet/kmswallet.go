package kmswallet

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	zsw "github.com/zhongshuwen/zswchain-go"
	ecc "github.com/zhongshuwen/zswchain-go/ecc"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	kms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/kms/v20190118"
)

type TencentKMSKeyBag struct {
	Keys                []*ecc.PrivateKey `json:"keys"`
	PublicKeyToKMSIdMap map[string]string `json:"publicKeyToKMSIdMap"`
	KMSClient           *kms.Client
}

func GetKMSClient(secretId string, secretKey string, region string, endpoint string) *kms.Client {

	credential := common.NewCredential(
		"SecretId",
		"SecretKey",
	)
	cpf := profile.NewClientProfile()
	cpf.HttpProfile.Endpoint = "kms.tencentcloudapi.com"
	client, _ := kms.NewClient(credential, "", cpf)
	return client
}
func NewTencentKMSKeyBag(client *kms.Client) *TencentKMSKeyBag {
	return &TencentKMSKeyBag{
		Keys:                make([]*ecc.PrivateKey, 0),
		PublicKeyToKMSIdMap: make(map[string]string),
		KMSClient:           client,
	}
}
func (b *TencentKMSKeyBag) AddKMSKeyById(kmsId string) (string, error) {
	result, err := b.KMSClient.GetPublicKey(&kms.GetPublicKeyRequest{
		KeyId: &kmsId,
	})
	if err != nil {
		return "", fmt.Errorf("Error adding KMS Key %w", err)
	}
	zswKey, err := ecc.SM2PemToZSWPublicKeyString([]byte(*result.Response.PublicKeyPem))
	if err != nil {
		return "", fmt.Errorf("Error adding KMS Key %w", err)
	}
	b.PublicKeyToKMSIdMap[zswKey] = kmsId
	return zswKey, nil
}

func (b *TencentKMSKeyBag) Add(wifKey string) error {
	privKey, err := ecc.NewPrivateKey(wifKey)
	if err != nil {
		return err
	}

	return b.Append(privKey)
}

func (b *TencentKMSKeyBag) Append(privateKey *ecc.PrivateKey) error {
	if privateKey == nil {
		return fmt.Errorf("appending a nil private key is forbidden")
	}

	b.Keys = append(b.Keys, privateKey)
	return nil
}

func (b *TencentKMSKeyBag) ImportFromFile(path string) error {
	inFile, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("import keys from file [%s], %s", path, err)
	}
	defer inFile.Close()
	scanner := bufio.NewScanner(inFile)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		key := strings.TrimSpace(strings.Split(scanner.Text(), " ")[0])

		if strings.Contains(key, "/") || strings.Contains(key, "#") || strings.Contains(key, ";") {
			return fmt.Errorf("lines should consist of a private key on each line, with an optional whitespace and comment")
		}

		if err := b.Add(key); err != nil {
			return err
		}
	}
	return nil
}

func (b *TencentKMSKeyBag) AvailableKeys(ctx context.Context) (out []ecc.PublicKey, err error) {
	for _, k := range b.Keys {
		out = append(out, k.PublicKey())
	}
	return
}

func (b *TencentKMSKeyBag) ImportPrivateKey(ctx context.Context, wifPrivKey string) (err error) {
	return b.Add(wifPrivKey)
}

func (b *TencentKMSKeyBag) ImportPrivateKeyFromEnv(ctx context.Context, envVarName string) error {
	var envValue = os.Getenv(envVarName)
	if len(envValue) == 0 {
		return fmt.Errorf("missing required private key (密钥) environmental variable: '%s'", envVarName)
	}
	var err = b.Add(envValue)
	if err != nil {
		return fmt.Errorf("invalid private key (密钥) environmental variable: '%s' (Error: %s)", envVarName, err)
	}
	return err
}

func (b *TencentKMSKeyBag) SignDigest(digest []byte, requiredKey ecc.PublicKey) (ecc.Signature, error) {

	privateKey := b.keyMap()[requiredKey.String()]
	if privateKey == nil {
		return ecc.Signature{}, fmt.Errorf("private key not found for public key [%s]", requiredKey.String())
	}

	return privateKey.Sign(digest)
}

func (b *TencentKMSKeyBag) Sign(ctx context.Context, tx *zsw.SignedTransaction, chainID []byte, requiredKeys ...ecc.PublicKey) (*zsw.SignedTransaction, error) {
	// TODO: probably want to use `tx.packed` and hash the ContextFreeData also.
	txdata, cfd, err := tx.PackedTransactionAndCFD()
	if err != nil {
		return nil, err
	}

	sigDigest := SigDigest(chainID, txdata, cfd)

	keyMap := b.keyMap()
	for _, key := range requiredKeys {
		privKey := keyMap[key.String()]
		if privKey != nil {

			sig, err := privKey.Sign(sigDigest)
			if err != nil {
				return nil, err
			}

			tx.Signatures = append(tx.Signatures, sig)
		} else if privKey == nil {
			kmsId := b.PublicKeyToKMSIdMap[key.String()]
			digest1 := base64.StdEncoding.EncodeToString(sigDigest)
			if kmsId != "" {
				digestText := "DIGEST"
				algoText := ("SM2DSA")
				resp, err := b.KMSClient.SignByAsymmetricKeyWithContext(ctx, &kms.SignByAsymmetricKeyRequest{

					Algorithm:   &algoText,
					KeyId:       &kmsId,
					Message:     &digest1,
					MessageType: &digestText,
				})
				if err != nil {
					return nil, fmt.Errorf("Signing request to kms failed %w", err)
				}
				decodedSig1, err := base64.StdEncoding.DecodeString(*resp.Response.Signature)

				if err != nil {
					return nil, fmt.Errorf("error decoding base64 signature from kms! %w", err)
				}
				pubKeyNew, err := ecc.NewPublicKey(key.String())

				if err != nil {
					return nil, fmt.Errorf("Error parsing pub key %w", err)
				}
				finalSig, err := pubKeyNew.GetCompoundPublicKeyASN1SignatureData([]byte(decodedSig1))
				if err != nil {
					return nil, fmt.Errorf("Error producing final sig for KMS signature %w", err)
				}
				tx.Signatures = append(tx.Signatures, *finalSig)

			} else {
				return nil, fmt.Errorf("private key for %q not in keybag", key)
			}
		}
	}
	// fmt.Println("Signing with", key.String(), privKey.String())
	// fmt.Println("SIGNING THIS DIGEST:", hex.EncodeToString(sigDigest))
	// fmt.Println("SIGNING THIS payload:", hex.EncodeToString(txdata))
	// fmt.Println("SIGNING THIS chainID:", hex.EncodeToString(chainID))
	// fmt.Println("SIGNING THIS cfd:", hex.EncodeToString(cfd))

	// tmpcnt, _ := json.Marshal(tx)
	// var newTx *SignedTransaction
	// _ = json.Unmarshal(tmpcnt, &newTx)

	return tx, nil
}

func (b *TencentKMSKeyBag) keyMap() map[string]*ecc.PrivateKey {
	out := map[string]*ecc.PrivateKey{}
	for _, key := range b.Keys {
		out[key.PublicKey().String()] = key
	}
	return out
}

func SigDigest(chainID, payload, contextFreeData []byte) []byte {
	h := sha256.New()
	if len(chainID) == 0 {
		_, _ = h.Write(make([]byte, 32, 32))
	} else {
		_, _ = h.Write(chainID)
	}
	_, _ = h.Write(payload)

	if len(contextFreeData) > 0 {
		h2 := sha256.New()
		_, _ = h2.Write(contextFreeData)
		_, _ = h.Write(h2.Sum(nil)) // add the hash of CFD to the payload
	} else {
		_, _ = h.Write(make([]byte, 32, 32))
	}
	return h.Sum(nil)
}

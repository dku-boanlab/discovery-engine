package license

import (
	"errors"
	"fmt"
	"github.com/accuknox/auto-policy-discovery/src/cluster"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mervick/aes-everywhere/go/aes256"
	"github.com/rs/zerolog/log"
	"k8s.io/client-go/kubernetes"
	"os"
	"strings"
	"time"
)

// For testing purpose
var publicKey = "-----BEGIN PUBLIC KEY-----\nMIGeMA0GCSqGSIb3DQEBAQUAA4GMADCBiAKBgHUc95xoPHqsuC3zLfCSHHJ9F/Gx\nlJdyBkns1wDYCLY8yX1vvZndfDP9br3dbFKOaYOYmF9e0gKcDpGItdBQe+TVX9ol\nM3S23yD/xHNKw+f88KjI0dPnj3IRgqajd5eBMhNNugRFzRKWBBLCflukm7CfjzUP\nX1jQ/NCkoTwjScpJAgMBAAE=\n-----END PUBLIC KEY-----"

const (
	discoveryEngineSecretName = "discovery-engine-license"
	licenseSecLabel           = "app=discovery-engine"
	namespace                 = "accuknox-agents"
)

// LicenseConfig to store configs required for licensing
type LicenseConfig struct {
	k8sClient *kubernetes.Clientset
	Tkn       *Token
	Lcs       *License
}

// License to store license related information
type License struct {
	UserId       string
	Key          string
	PlatformUUID string
}

var LCfg *LicenseConfig

// InitializeConfig to initialize license config
func InitializeConfig(k8sClient *kubernetes.Clientset) {
	LCfg = &LicenseConfig{
		k8sClient: k8sClient,
		Tkn:       nil,
		Lcs:       nil,
	}
}

// CheckLicenseSecret to fetch license secret and validate
func CheckLicenseSecret() error {
	log.Info().Msgf("fetching license secrets to validate discovery-engine licensing")
	secret, err := cluster.GetSecrets(LCfg.k8sClient, licenseSecLabel, namespace, discoveryEngineSecretName)
	if err != nil {
		log.Error().Msgf("error while fetching secrets for discovery engine licensing, error: %s", err.Error())
		return err
	}
	if secret == nil {
		return errors.New("license secret doesn't exist for discovery-engine")
	}

	l := &License{
		UserId: string(secret.Data["user-id"]),
		Key:    string(secret.Data["key"]),
	}
	err = l.ValidateLicense()
	// Initialize to global config only after validation is done.
	LCfg.Lcs = l
	if err != nil {
		log.Error().Msgf("error while validating license retrieved through secrets, error: %s", err.Error())
		return err
	}

	log.Info().Msgf("license validation successfully for user-id: %s with key: %s", LCfg.Lcs.UserId, LCfg.Lcs.Key)
	return nil
}

// ValidateLicense to validate license
func (l *License) ValidateLicense() error {
	var err error

	var existingLicense = LCfg.checkExistingLicense()

	if existingLicense {
		if !LCfg.Tkn.checkExpiration() {
			err = fmt.Errorf("valid license already exists with user-id: %s, key: %s and platform uuid: %s", LCfg.Lcs.UserId, LCfg.Lcs.Key, LCfg.Lcs.PlatformUUID)
			log.Error().Msgf("%s", err)
			return err
		}
		log.Info().Msgf("expired license already exists, updating the license")
		// Not required, since we are not removing secrets before validation
		//err = LCfg.removeLicenseSecretsConfig()
		//if err != nil {
		//	return err
		//}
	}

	l.PlatformUUID, err = LCfg.getKubeSystemUUID()
	if err != nil {
		log.Error().Msgf("error while fetching uuid of kube-system namespace, error: %s", err.Error())
		return err
	}

	decryptedKey, err := decryptKey(l.Key, l.PlatformUUID)
	if err != nil {
		log.Error().Msgf("error while decrypting license key, error: %s", err.Error())
		return err
	}

	t, err := validateToken(decryptedKey, l.UserId)
	if err != nil {
		log.Error().Msgf("error while validating jwt token")
		return err
	}

	log.Info().Msgf("license validation successfully for user: %s with license key: %s", l.UserId, l.Key)

	// To remove existing secret and create a new secret to store license
	if existingLicense {
		err = LCfg.removeLicenseSecretsConfig()
		if err != nil {
			log.Error().Msgf("error while deleting secrets for license, error: %s", err.Error())
			return err
		}
	}

	secret, err := cluster.CreateLicenseSecret(LCfg.k8sClient, namespace, l.Key, l.UserId, discoveryEngineSecretName, licenseSecLabel)
	if err != nil {
		log.Error().Msgf("error while creating secret for discovery engine license, error: %s", err.Error())
		return err
	}
	// Initialize to global config only after validation is done.
	LCfg.Lcs = l
	LCfg.Tkn = t

	log.Info().Msgf("secret for discovery engine license with name: %s and uuid: %s", secret.GetName(), secret.GetUID())
	log.Info().Msgf("license installed successfully")
	return nil
}

func (cfg *LicenseConfig) getKubeSystemUUID() (string, error) {
	uuid, err := cluster.GetKubeSystemUUID(cfg.k8sClient)
	if err != nil {
		log.Error().Msgf("error while fetching uuid of kube-system namespace, error: %s", err.Error())
		return "", err
	}
	return uuid, nil
}

func decryptKey(key string, platformUUID string) (string, error) {
	decryptedKey := aes256.Decrypt(key, platformUUID)
	tokenSplit := strings.Split(decryptedKey, ".")
	if len(tokenSplit) != 3 {
		log.Error().Msgf("invalid licence key")
		return "", errors.New("invalid license key")
	}
	return decryptedKey, nil
}

type Token struct {
	jwt    *jwt.Token
	claims *Claims
}

type Claims struct {
	Features []string `json:"features"`
	*jwt.RegisteredClaims
}

func validateToken(decryptedKey string, userId string) (*Token, error) {

	claims := &Claims{}

	key, err := jwt.ParseRSAPublicKeyFromPEM([]byte(publicKey))
	if err != nil {
		return nil, fmt.Errorf("error parsing RSA public key: %v\n", err)
	}

	jwtToken, err := jwt.ParseWithClaims(decryptedKey, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return key, nil
	})
	if err != nil {
		log.Error().Msgf("error while parsing jwt token, error: %s", err.Error())
		return nil, err
	}

	t := &Token{
		jwt:    jwtToken,
		claims: claims,
	}

	err = t.validateClaims(userId)
	if err != nil {
		log.Error().Msgf("error while validating claims of jwt token, error: %s", err.Error())
		return nil, err
	}

	return t, err
}

func (t *Token) validateClaims(userId string) error {
	return t.validateUserId(userId)
}

func (t *Token) validateUserId(userId string) error {
	sub, err := t.claims.GetSubject()
	if err != nil {
		log.Error().Msgf("error while getting subject from jwt token, error: %s", err.Error())
		return err
	}

	if sub != userId {
		log.Error().Msgf("error while validating userId")
		return errors.New("error while validating userId")
	}
	return nil

}

func (t *Token) getFeatures() ([]string, error) {
	return t.claims.Features, nil
}

// WatchFeatures to validate license and watch features
func (cfg *LicenseConfig) WatchFeatures() bool {

	for {
		time.Sleep(5 * time.Second)
		if cfg.Lcs == nil || cfg.Tkn == nil {
			continue
		}

		if !cfg.Tkn.checkExpiration() {
			log.Info().Msgf("valid license exists for discovery-engine")
			return true
		}
	}

}

func (t *Token) checkExpiration() bool {
	if t == nil {
		log.Error().Msgf("error while validating license, token doesn't exists")
		return true
	}
	exp, err := t.claims.RegisteredClaims.GetExpirationTime()
	if err != nil {
		log.Error().Msgf("error while getting expiration time for license, error: %s", err.Error())
		return true
	}
	if exp.Before(time.Now()) {
		return true
	}
	return false
}

func (cfg *LicenseConfig) checkExistingLicense() bool {
	return cfg.Lcs != nil || cfg.Tkn != nil
}

func (cfg *LicenseConfig) removeLicenseSecretsConfig() error {
	cfg.Lcs = nil
	cfg.Tkn = nil
	return cluster.DeleteSecrets(LCfg.k8sClient, discoveryEngineSecretName, namespace)
}

func (cfg *LicenseConfig) WatchLicenseValidity() {
	for {
		if cfg.Lcs == nil || cfg.Tkn == nil {
			log.Error().Msgf("license doesn't exists for discovery-engine")
			os.Exit(1)
		}

		if cfg.Tkn.checkExpiration() {
			log.Error().Msgf("license got expired, get a new license")
			// Not required, since we are not deleting secrets at the time of expiry. Can be used to get status
			//err := cfg.removeLicenseSecretsConfig()
			//if err != nil {
			//	log.Error().Msgf("error while deleting secrets for license")
			//}
			os.Exit(1)
		}
		time.Sleep(15 * time.Second)
	}
}

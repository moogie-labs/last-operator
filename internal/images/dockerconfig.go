package images

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type dockerAuth struct {
	Auth          string `json:"auth,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
}

type dockerConfigFile struct {
	Auths map[string]dockerAuth `json:"auths"`
}

// DockerConfig builds a config.json holding one basic credential for host (bundled mode).
func DockerConfig(host, user, password string) []byte {
	raw, err := json.Marshal(dockerConfigFile{Auths: map[string]dockerAuth{
		host: {Auth: base64.StdEncoding.EncodeToString([]byte(user + ":" + password))},
	}})
	if err != nil { // a map of strings always marshals
		panic(err)
	}
	return raw
}

// decodeAuth returns the user and password of an "auth" value (base64 "user:password").
// Error text never includes the value.
func decodeAuth(auth string) (string, string, error) {
	raw, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "", errors.New("auth is not base64")
	}
	user, password, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", errors.New(`auth is not "user:password"`)
	}
	return user, password, nil
}

// keyHost is the host of a config.json key, which may carry a scheme and/or a path
// ("https://host", "host/v1/").
func keyHost(key string) string {
	if _, rest, ok := strings.Cut(key, "://"); ok {
		key = rest
	}
	host, _, _ := strings.Cut(key, "/")
	return host
}

// CredentialsFor returns the basic credentials a Docker config JSON holds for host
// ("auths"[key].auth, base64 "user:pass"), or nil when it has none. An exact key
// match wins; otherwise a key that is host with an "http(s)://" scheme and/or a
// path ("https://host", "host/v1/") matches. The password may contain colons.
func CredentialsFor(dockerConfig []byte, host string) (*Credentials, error) {
	var cfg dockerConfigFile
	if err := json.Unmarshal(dockerConfig, &cfg); err != nil {
		return nil, errors.New("docker config is not valid JSON")
	}
	entry, ok := cfg.Auths[host]
	if !ok {
		for key, e := range cfg.Auths {
			if keyHost(key) == host {
				entry, ok = e, true
				break
			}
		}
	}
	if !ok {
		return nil, nil
	}
	if entry.Auth == "" {
		if entry.Username != "" || entry.Password != "" {
			return &Credentials{Username: entry.Username, Password: entry.Password}, nil
		}
		return nil, nil
	}
	user, password, err := decodeAuth(entry.Auth)
	if err != nil {
		return nil, fmt.Errorf("docker config entry for %s: %w", host, err)
	}
	return &Credentials{Username: user, Password: password}, nil
}

// pushSecrets validates a push config and returns every secret in it, for redaction:
// each auth string, its decoded "user:password" and the password, plus any plain
// password or token field.
func pushSecrets(dockerConfig []byte) ([]string, error) {
	var cfg dockerConfigFile
	if err := json.Unmarshal(dockerConfig, &cfg); err != nil {
		return nil, errors.New("push config is not valid JSON")
	}
	var secrets []string
	for host, e := range cfg.Auths {
		secrets = append(secrets, e.Password, e.IdentityToken, e.RegistryToken)
		if e.Auth == "" {
			continue
		}
		user, password, err := decodeAuth(e.Auth)
		if err != nil {
			return nil, fmt.Errorf("push config entry for %s: %w", host, err)
		}
		secrets = append(secrets, e.Auth, user+":"+password, password)
	}
	return secrets, nil
}

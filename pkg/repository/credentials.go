package repository

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"golang.org/x/exp/maps"
	"gopkg.in/yaml.v3"
)

type RepositoryCreds map[string]string

func (creds RepositoryCreds) AsBytesMap() map[string][]byte {
	result := map[string][]byte{}

	for key, value := range creds {
		result[key] = []byte(value)
	}
	return result
}

func (creds RepositoryCreds) expandEnvVars() {
	for _, key := range maps.Keys(creds) {
		value := creds[key]
		if rest, found := strings.CutPrefix(value, "$"); found && len(rest) > 0 {
			creds[key] = os.Getenv(rest)
		}
	}
}

type Credentials map[string]RepositoryCreds

func ReadCredentials(input io.Reader) (Credentials, error) {
	bytes, err := io.ReadAll(input)
	if err != nil {
		return nil, fmt.Errorf("unable to read input: %w", err)
	}

	credentials := Credentials{}
	err = yaml.Unmarshal(bytes, credentials)
	if err != nil {
		return nil, fmt.Errorf("unable to parse credentials YAML: %w", err)
	}

	for _, value := range credentials {
		value.expandEnvVars()
	}
	return credentials, nil
}

func (credentials Credentials) FindForRepo(
	repoURL *url.URL,
) (*RepositoryCreds, error) {
	if creds, ok := credentials[repoURL.String()]; ok {
		return &creds, nil
	}
	for storedRepoURL, creds := range credentials {
		parsedURL, err := url.Parse(storedRepoURL)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to parse configured repository URL %s:%w",
				storedRepoURL,
				err,
			)
		}
		if repoURL.Scheme == parsedURL.Scheme &&
			repoURL.Host == parsedURL.Host &&
			repoURL.User.Username() == parsedURL.User.Username() {
			return &creds, nil
		}
	}
	return nil, nil
}

// Copyright © The Sage Group plc or its licensors.

package yaml

import (
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/kustomize/kyaml/yaml"
)

func GetGroup(node *yaml.RNode) string {
	apiVersion := node.GetApiVersion()
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[0]
}

func GetStringOr(
	node *yaml.RNode,
	fieldSpec string,
	defaultValue string,
) (string, error) {
	result, err := node.GetString(fieldSpec)
	if err != nil && errors.Is(err, yaml.NoFieldError{Field: fieldSpec}) {
		return defaultValue, nil
	}
	if err != nil {
		return defaultValue, fmt.Errorf("unable to get %s: %w", fieldSpec, err)
	}
	return result, nil
}

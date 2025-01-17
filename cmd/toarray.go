// Copyright © The Sage Group plc or its licensors.

package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

type ToArrayCommandOptions struct {
	topLevelAttribute string
}

const ToArrayCommandName = "toarray"

func convertToYamlArray(
	input io.Reader,
	output io.Writer,
	topLevelAttribute string,
) error {
	reader := &kio.ByteReader{
		Reader:                input,
		OmitReaderAnnotations: true,
	}
	nodes, err := reader.Read()
	if err != nil {
		return fmt.Errorf("unable to read input: %w", err)
	}
	content := make([]*yaml.Node, 0, len(nodes))
	for _, node := range nodes {
		content = append(content, node.YNode())
	}
	nodes = []*yaml.RNode{
		yaml.NewRNode(&yaml.Node{
			Kind: yaml.MappingNode,
			Content: []*yaml.Node{
				{
					Kind:  yaml.ScalarNode,
					Value: topLevelAttribute,
				},
				{
					Kind:    yaml.SequenceNode,
					Content: content,
				},
			},
		}),
	}

	writer := kio.ByteWriter{Writer: output}
	err = writer.Write(nodes)
	if err != nil {
		return fmt.Errorf("unable to write output: %w", err)
	}
	return nil
}

func NewToArrayCommand(options *ToArrayCommandOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   ToArrayCommandName,
		Short: "Converts YAML documents in the input to a single document with an array of objects",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, logger := getContextAndLogger(cmd)
			logger.Info(fmt.Sprintf("Staring %s command", cmd.Use))
			input, err := getYAMLInputReader(args)
			if err != nil {
				return err
			}
			defer input.Close()
			return convertToYamlArray(input, os.Stdout, options.topLevelAttribute)
		},
		SilenceUsage: true,
	}
	command.PersistentFlags().StringVarP(
		&options.topLevelAttribute,
		"top-level-attribute",
		"",
		"objects",
		"Name of the top-level array attribute",
	)

	return command
}

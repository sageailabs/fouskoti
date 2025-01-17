// Copyright © The Sage Group plc or its licensors.

package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
)

func TestAll(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	format.TruncatedDiff = false
	ginkgo.RunSpecs(t, "toarray Test Suite")
}

var _ = ginkgo.Describe("toarray command", func() {
	var g gomega.Gomega

	ginkgo.BeforeEach(func() {
		g = gomega.NewWithT(ginkgo.GinkgoT())
	})

	ginkgo.It("converts YAML documents to array", func() {
		repoRoot, err := os.MkdirTemp("", "")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer os.RemoveAll(repoRoot)

		input := strings.Join([]string{
			"a: b",
			"---",
			"c: d",
		}, "\n")
		output := &bytes.Buffer{}
		err = convertToYamlArray(bytes.NewBufferString(input), output, "toplevel")
		g.Expect(err).ToNot(gomega.HaveOccurred())

		g.Expect(output.String()).To(gomega.Equal(strings.Join([]string{
			"toplevel:",
			"- a: b",
			"- c: d",
			"",
		}, "\n"),
		))
	})
})

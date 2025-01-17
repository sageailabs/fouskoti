// Copyright © The Sage Group plc or its licensors.

package repository

import (
	"bytes"
	"os"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("repository credentials", func() {
	var g gomega.Gomega

	ginkgo.BeforeEach(func() {
		g = gomega.NewWithT(ginkgo.GinkgoT())
	})

	ginkgo.It("load from provided stream", func() {
		input := bytes.NewBufferString(strings.Join([]string{
			"ssh://git@github.com/:",
			"  credentials:",
			"    identity: |",
			"      -----BEGIN OPENSSH PRIVATE KEY-----",
			"      <snip>",
			"      -----END OPENSSH PRIVATE KEY-----",
			"    known_hosts: |",
			"      github.com ssh-ed25519 <pubic-key>",
		}, "\n"))
		creds, err := ReadCredentials(input)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(creds).To(gomega.HaveLen(1))
		g.Expect(creds).To(gomega.HaveKey("ssh://git@github.com/"))
		repoCreds := creds["ssh://git@github.com/"]
		g.Expect(repoCreds.Credentials).To(gomega.HaveKeyWithValue(
			"identity",
			gomega.And(
				gomega.HavePrefix("-----BEGIN OPENSSH PRIVATE KEY-----\n"),
				gomega.HaveSuffix("-----END OPENSSH PRIVATE KEY-----\n"),
			),
		))
		g.Expect(repoCreds.Credentials).To(gomega.HaveKeyWithValue(
			"known_hosts",
			"github.com ssh-ed25519 <pubic-key>",
		))
	})

	ginkgo.It("expand environment variables", func() {
		saved := os.Getenv("GITHUB_TOKEN")
		defer os.Setenv("GITHUB_TOKEN", saved)
		os.Setenv("GITHUB_TOKEN", "foo")
		input := bytes.NewBufferString(strings.Join([]string{
			"https://github.com/:",
			"  credentials:",
			"    token: $GITHUB_TOKEN",
		}, "\n"))
		creds, err := ReadCredentials(input)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(creds).To(gomega.HaveKey("https://github.com/"))
		repoCreds := creds["https://github.com/"]
		g.Expect(repoCreds.Credentials).To(gomega.HaveKeyWithValue(
			"token",
			"foo",
		))
	})
})

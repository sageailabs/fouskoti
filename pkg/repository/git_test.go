package repository

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/fluxcd/pkg/git"
	"github.com/fluxcd/pkg/git/gogit"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
)

var _ = ginkgo.Describe("GitRepository expansion", func() {
	var g gomega.Gomega
	var ctx context.Context
	var logger *slog.Logger

	chartFiles := map[string]string{
		"Chart.yaml": strings.Join([]string{
			"apiVersion: v2",
			"name: test-chart",
			"version: 0.1.0",
		}, "\n"),
		"values.yaml": strings.Join([]string{
			"data:",
			"  foo: bar",
		}, "\n"),
		"templates/configmap.yaml": strings.Join([]string{
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: {{ .Release.Namespace }}",
			"  name: {{ .Release.Name }}-configmap",
			"data: {{- .Values.data | toYaml | nindent 2 }}",
		}, "\n"),
	}

	ginkgo.BeforeEach(func() {
		g = gomega.NewWithT(ginkgo.GinkgoT())
		ctx = context.Background()
		handler := slog.NewTextHandler(
			ginkgo.GinkgoWriter,
			&slog.HandlerOptions{AddSource: true, Level: slog.LevelDebug},
		)
		logger = slog.New(handler)
	})

	ginkgo.It("expands HelmRelease from a chart in a repository", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns",
			"  name: test",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: charts/test-chart",
			"      sourceRef:",
			"        kind: GitRepository",
			"        name: local",
			"  values:",
			"    data:",
			"      foo: baz",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: GitRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			"  url: " + repoURL,
		}, "\n")

		gitClient := &GitClientMock{}
		gitClient.
			On("Clone", mock.Anything, repoURL, mock.Anything).
			Run(func(mock.Arguments) {
				err := createFileTree(path.Join(repoRoot, "charts/test-chart"), chartFiles)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}).
			Return(&git.Commit{Hash: git.Hash("dummy")}, nil)
		expander := NewHelmReleaseExpander(
			ctx,
			logger,
			func(
				path string,
				authOpts *git.AuthOptions,
				clientOpts ...gogit.ClientOption,
			) (GitClientInterface, error) {
				repoRoot = path
				return gitClient, nil
			},
			nil,
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			"",
			false,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(output.String()).To(gomega.Equal(strings.Join([]string{
			input,
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test-configmap",
			"data:",
			"  foo: baz",
			"",
		}, "\n"),
		))
	})

	// Verifies that the repository files will be reused and not cloned twice,
	// even when referred from two different GitRepository resources but with the
	// same repository URL.
	ginkgo.It("caches charts from repository in memory", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns",
			"  name: test",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: charts/test-chart",
			"      sourceRef:",
			"        kind: GitRepository",
			"        name: local",
			"  values:",
			"    data:",
			"      foo: baz",
			"---",
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns-other",
			"  name: test-another",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: charts/test-chart",
			"      sourceRef:",
			"        kind: GitRepository",
			"        name: local-2",
			"  values:",
			"    data:",
			"      foo: baz",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: GitRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			"  url: " + repoURL,
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: GitRepository",
			"metadata:",
			"  namespace: testns-other",
			"  name: local-2",
			"spec:",
			"  url: " + repoURL, // Same repository URL.
		}, "\n")

		gitClient := &GitClientMock{}
		gitClient.
			On("Clone", mock.Anything, repoURL, mock.Anything).
			Once(). // Clone is attempted only once.
			Run(func(mock.Arguments) {
				err := createFileTree(path.Join(repoRoot, "charts/test-chart"), chartFiles)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}).
			Return(&git.Commit{Hash: git.Hash("dummy")}, nil)
		expander := NewHelmReleaseExpander(
			ctx,
			logger,
			func(
				path string,
				authOpts *git.AuthOptions,
				clientOpts ...gogit.ClientOption,
			) (GitClientInterface, error) {
				repoRoot = path
				return gitClient, nil
			},
			nil,
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			"",
			true,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(output.String()).To(gomega.Equal(strings.Join([]string{
			input,
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test-configmap",
			"data:",
			"  foo: baz",
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns-other",
			"  name: testns-other-test-another-configmap",
			"data:",
			"  foo: baz",
			"",
		}, "\n"),
		))
	})

	ginkgo.It("uses file cache when provided", func() {
		cacheRoot, err := os.MkdirTemp("", "")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer os.RemoveAll(cacheRoot)

		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns",
			"  name: test",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: charts/test-chart",
			"      sourceRef:",
			"        kind: GitRepository",
			"        name: local",
			"  values:",
			"    data:",
			"      foo: baz",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: GitRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			"  url: " + repoURL,
		}, "\n")

		gitClient := &GitClientMock{}
		// The call should only happen once even with two calls to ExpandHelmReleases.
		gitClient.
			On("Clone", mock.Anything, repoURL, mock.Anything).
			Once().
			Run(func(mock.Arguments) {
				err := createFileTree(path.Join(repoRoot, "charts/test-chart"), chartFiles)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}).
			Return(&git.Commit{Hash: git.Hash("dummy")}, nil)
		expander := NewHelmReleaseExpander(
			ctx,
			logger,
			func(
				path string,
				authOpts *git.AuthOptions,
				clientOpts ...gogit.ClientOption,
			) (GitClientInterface, error) {
				repoRoot = path
				return gitClient, nil
			},
			nil,
		)
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			cacheRoot,
			false,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(output.String()).To(gomega.Equal(strings.Join([]string{
			input,
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test-configmap",
			"data:",
			"  foo: baz",
			"",
		}, "\n"),
		))

		// Instantiate a second, independent instance of HelmReleaseExpander.
		expander = NewHelmReleaseExpander(
			ctx,
			logger,
			func(
				path string,
				authOpts *git.AuthOptions,
				clientOpts ...gogit.ClientOption,
			) (GitClientInterface, error) {
				repoRoot = path
				return gitClient, nil
			},
			nil,
		)
		output = &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			cacheRoot,
			false,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(output.String()).To(gomega.Equal(strings.Join([]string{
			input,
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test-configmap",
			"data:",
			"  foo: baz",
			"",
		}, "\n"),
		))
		chartDir := filepath.Join(
			cacheRoot,
			"ssh:##git@localhost#dummy.git/master####/charts/test-chart",
		)
		g.Expect(chartDir).To(gomega.BeADirectory())
		configmapTemplateName := filepath.Join(chartDir, "templates/configmap.yaml")
		g.Expect(configmapTemplateName).To(gomega.BeARegularFile())
		configmapTemplate, err := os.ReadFile(configmapTemplateName)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(string(configmapTemplate)).To(
			gomega.Equal(chartFiles["templates/configmap.yaml"]))
	})

	ginkgo.It("handles relative dependency chart paths", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns",
			"  name: test",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: charts/test-chart",
			"      sourceRef:",
			"        kind: GitRepository",
			"        name: local",
			"  values:",
			"    data:",
			"      foo: baz",
			"    dependency-chart:",
			"      data:",
			"        foo: bar",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: GitRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			"  url: " + repoURL,
		}, "\n")

		chartFiles := map[string]string{
			"test-chart/Chart.yaml": strings.Join([]string{
				"apiVersion: v2",
				"name: test-chart",
				"version: 0.1.0",
				"dependencies:",
				"- name: dependency-chart",
				"  version: ^0.1.0",
				"  repository: ../dependency-chart",
			}, "\n"),
			"test-chart/values.yaml": strings.Join([]string{
				"data:",
				"  foo: bar",
			}, "\n"),
			"test-chart/templates/configmap.yaml": strings.Join([]string{
				"apiVersion: v1",
				"kind: ConfigMap",
				"metadata:",
				"  namespace: {{ .Release.Namespace }}",
				"  name: {{ .Release.Name }}-configmap",
				"data: {{- .Values.data | toYaml | nindent 2 }}",
			}, "\n"),
			"dependency-chart/Chart.yaml": strings.Join([]string{
				"apiVersion: v2",
				"name: dependency-chart",
				"version: 0.1.0",
			}, "\n"),
			"dependency-chart/values.yaml": strings.Join([]string{
				"data:",
				"  foo: bar",
			}, "\n"),
			"dependency-chart/templates/configmap.yaml": strings.Join([]string{
				"apiVersion: v1",
				"kind: ConfigMap",
				"metadata:",
				"  namespace: {{ .Release.Namespace }}",
				"  name: {{ .Release.Name }}-dependency-configmap",
				"data: {{- .Values.data | toYaml | nindent 2 }}",
			}, "\n"),
		}

		gitClient := &GitClientMock{}
		gitClient.
			On("Clone", mock.Anything, repoURL, mock.Anything).
			Run(func(mock.Arguments) {
				err := createFileTree(path.Join(repoRoot, "charts"), chartFiles)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}).
			Return(&git.Commit{Hash: git.Hash("dummy")}, nil)
		expander := NewHelmReleaseExpander(
			ctx,
			logger,
			func(
				path string,
				authOpts *git.AuthOptions,
				clientOpts ...gogit.ClientOption,
			) (GitClientInterface, error) {
				repoRoot = path
				return gitClient, nil
			},
			nil,
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			"",
			false,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(output.String()).To(gomega.Equal(strings.Join([]string{
			input,
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test-configmap",
			"data:",
			"  foo: baz",
			"---",
			"# Source: test-chart/charts/dependency-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test-dependency-configmap",
			"data:",
			"  foo: bar",
			"",
		}, "\n"),
		))
	})
})

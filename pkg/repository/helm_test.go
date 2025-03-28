package repository

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("HelmRepository expansion", func() {
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
		repoRoot, err := os.MkdirTemp("", "")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer os.RemoveAll(repoRoot)
		server, port, serverDone, err := serveDirectory(repoRoot, logger, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		err = createSingleChartHelmRepository(
			"test-chart",
			"0.1.0",
			chartFiles,
			port,
			repoRoot,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns",
			"  name: test",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: test-chart",
			"      version: \">=0.1.0\"",
			"      sourceRef:",
			"        kind: HelmRepository",
			"        name: local",
			"  values:",
			"    data:",
			"      foo: baz",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: HelmRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			fmt.Sprintf("  url: http://localhost:%d", port),
		}, "\n")

		expander := NewHelmReleaseExpander(ctx, logger, nil, nil)
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			1,
			"",
			false,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		err = stopServing(server, serverDone)
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

	ginkgo.It("caches charts from repository in memory", func() {
		repoRoot, err := os.MkdirTemp("", "")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer os.RemoveAll(repoRoot)
		recorder := logRecorder{}
		server, port, serverDone, err := serveDirectory(repoRoot, logger, &recorder)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		err = createSingleChartHelmRepository(
			"test-chart",
			"0.1.0",
			chartFiles,
			port,
			repoRoot,
		)
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns",
			"  name: test",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: test-chart",
			"      version: \">=0.1.0\"",
			"      sourceRef:",
			"        kind: HelmRepository",
			"        name: local",
			"  values:",
			"    data:",
			"      foo: baz",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: HelmRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			fmt.Sprintf("  url: http://localhost:%d", port),
			"---",
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns-different",
			"  name: test-another",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: test-chart",
			"      version: \">=0.0.1\"",
			"      sourceRef:",
			"        kind: HelmRepository",
			"        name: local-other",
			"  values:",
			"    data:",
			"      foo: baz",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: HelmRepository",
			"metadata:",
			"  namespace: testns-different",
			"  name: local-other",
			"spec:",
			fmt.Sprintf("  url: http://localhost:%d", port),
		}, "\n")
		g.Expect(err).ToNot(gomega.HaveOccurred())

		expander := NewHelmReleaseExpander(ctx, logger, nil, nil)
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			1,
			"",
			true,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		err = stopServing(server, serverDone)
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
			"  namespace: testns-different",
			"  name: testns-different-test-another-configmap",
			"data:",
			"  foo: baz",
			"",
		}, "\n"),
		))
		g.Expect(recorder.records).To(gomega.HaveLen(3))
		g.Expect(recorder.records[0]).To(gomega.HaveField("URL.Path", "/index.yaml"))
		// Only one chart request is expected.
		g.Expect(recorder.records[1]).To(gomega.HaveField("URL.Path", "/test-chart-0.1.0.tgz"))
		g.Expect(recorder.records[2]).To(gomega.HaveField("URL.Path", "/index.yaml"))
	})

	ginkgo.It("uses file cache when provided", func() {
		cacheRoot, err := os.MkdirTemp("", "")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer os.RemoveAll(cacheRoot)
		repoRoot, err := os.MkdirTemp("", "")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer os.RemoveAll(repoRoot)
		server, port, serverDone, err := serveDirectory(repoRoot, logger, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		err = createSingleChartHelmRepository(
			"test-chart",
			"0.1.0",
			chartFiles,
			port,
			repoRoot,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns",
			"  name: test",
			"spec:",
			"  chart:",
			"    spec:",
			"      chart: test-chart",
			"      version: \">=0.1.0\"",
			"      sourceRef:",
			"        kind: HelmRepository",
			"        name: local",
			"  values:",
			"    data:",
			"      foo: baz",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1",
			"kind: HelmRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			fmt.Sprintf("  url: http://localhost:%d", port),
		}, "\n")

		expander := NewHelmReleaseExpander(ctx, logger, nil, nil)
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			1,
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
		err = stopServing(server, serverDone)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		expander = NewHelmReleaseExpander(ctx, logger, nil, nil)
		output = &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			1,
			cacheRoot,
			false,
		)
		// The operation should succeed even though ExpandHelmReleases does not have
		// access to the chart server (it has been stopped). The chart should be
		// loaded from the file cache.
		g.Expect(err).ToNot(gomega.HaveOccurred())
		repoDir := filepath.Join(cacheRoot, fmt.Sprintf("http:##localhost:%d", port))
		g.Expect(repoDir).To(gomega.BeADirectory())
		g.Expect(filepath.Join(repoDir, "repo-index.yaml")).To(gomega.BeARegularFile())
		configmapTemplateName := filepath.Join(
			repoDir,
			"test-chart-0.1.0/templates/configmap.yaml",
		)
		g.Expect(configmapTemplateName).To(gomega.BeARegularFile())
		configmapTemplate, err := os.ReadFile(configmapTemplateName)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(string(configmapTemplate)).To(
			gomega.Equal(chartFiles["templates/configmap.yaml"]))
	})

})

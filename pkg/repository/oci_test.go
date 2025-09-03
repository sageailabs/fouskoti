package repository

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
)

type repoClientMock struct {
	mock.Mock
}

func (mock *repoClientMock) Login(
	registryHost string,
	username string,
	password string,
) error {
	args := mock.Called(registryHost, username, password)
	return args.Error(0)
}

func (mock *repoClientMock) Tags(chartRef string) ([]string, error) {
	args := mock.Called(chartRef)
	return args.Get(0).([]string), args.Error(1)
}

func (mock *repoClientMock) Get(chartRef string) (*bytes.Buffer, error) {
	args := mock.Called(chartRef)
	return args.Get(0).(*bytes.Buffer), args.Error(1)
}

var _ = ginkgo.Describe("OCIRepository expansion", func() {
	var g gomega.Gomega
	var ctx context.Context
	var logger *slog.Logger
	var chartArchive []byte

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

		repoRoot, err := os.MkdirTemp("", "")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer os.RemoveAll(repoRoot)
		chartArchiveBuffer, err := createChartArchive("test-chart", "0.1.0", chartFiles)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		chartArchive = chartArchiveBuffer.Bytes()
	})

	ginkgo.It("expands HelmRelease from a chart in a repository", func() {
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
			"  type: oci",
			"  insecure: true",
			"  url: oci://localhost:8888",
		}, "\n")

		repoClient := &repoClientMock{}
		repoClient.
			On("Tags", "localhost:8888/test-chart").
			Return([]string{"0.1.0"}, nil)
		repoClient.
			On("Get", "localhost:8888/test-chart:0.1.0").
			Return(bytes.NewBuffer(chartArchive), nil)

		expander := NewHelmReleaseExpander(
			ctx,
			logger,
			nil,
			func(insecure bool) (repositoryClient, error) {
				return repoClient, nil
			},
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			nil,
			1,
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

	ginkgo.It("caches charts from repository in memory", func() {
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
			"      foo: bar",
			"---",
			"apiVersion: helm.toolkit.fluxcd.io/v2",
			"kind: HelmRelease",
			"metadata:",
			"  namespace: testns",
			"  name: test2",
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
			"  type: oci",
			"  insecure: true",
			"  url: oci://localhost:8888",
		}, "\n")

		repoClient := &repoClientMock{}
		// The tags are requested twice - once for each HelmRelease being expanded.
		// But Get is invoked only once. For the second HelmRelease, the
		// memory-cached chart should be used.
		repoClient.
			On("Tags", "localhost:8888/test-chart").
			Twice().
			Return([]string{"0.1.0"}, nil)
		repoClient.
			On("Get", "localhost:8888/test-chart:0.1.0").
			Once().
			Return(bytes.NewBuffer(chartArchive), nil)

		expander := NewHelmReleaseExpander(
			ctx,
			logger,
			nil,
			func(insecure bool) (repositoryClient, error) {
				return repoClient, nil
			},
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			nil,
			1,
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
			"  foo: bar",
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test2-configmap",
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
			"  type: oci",
			"  insecure: true",
			"  url: oci://localhost:8888",
		}, "\n")

		repoClient := &repoClientMock{}
		// The tags are requested twice - once for each invocation of
		// ExpandHelmReleases, but Get is invoked only once. For the second
		// invocation of ExpandHelmReleases, the cached files should be used.
		repoClient.
			On("Tags", "localhost:8888/test-chart").
			Twice().
			Return([]string{"0.1.0"}, nil)
		repoClient.
			On("Get", "localhost:8888/test-chart:0.1.0").
			Once().
			Return(bytes.NewBuffer(chartArchive), nil)

		expander := NewHelmReleaseExpander(
			ctx,
			logger,
			nil,
			func(insecure bool) (repositoryClient, error) {
				return repoClient, nil
			},
		)
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			io.Discard,
			nil,
			nil,
			nil,
			1,
			cacheRoot,
			false,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		expander = NewHelmReleaseExpander(
			ctx,
			logger,
			nil,
			func(insecure bool) (repositoryClient, error) {
				return repoClient, nil
			},
		)
		configmapTemplateName := filepath.Join(
			cacheRoot,
			"oci:##localhost:8888/test-chart-0.1.0/templates/configmap.yaml",
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		// Spot check the cached chart files.
		g.Expect(configmapTemplateName).To(gomega.BeARegularFile())
		configmapTemplate, err := os.ReadFile(configmapTemplateName)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(string(configmapTemplate)).To(
			gomega.Equal(chartFiles["templates/configmap.yaml"]))

		// Run the expansion a second time.
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
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
	})
})

// Copyright © The Sage Group plc or its licensors.

package repository

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fluxcd/pkg/git"
	"github.com/fluxcd/pkg/git/gogit"
	"github.com/fluxcd/pkg/git/repository"
	"github.com/gorilla/handlers"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/stretchr/testify/mock"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/repo"
)

func createFileTree(treeRoot string, files map[string]string) error {
	for filePath, content := range files {
		fullPath := path.Join(treeRoot, filePath)
		if err := os.MkdirAll(path.Dir(fullPath), 0755); err != nil {
			return fmt.Errorf(
				"unable to creare directory for test file %s: %w",
				filePath,
				err,
			)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return fmt.Errorf(
				"unable to write test file %s: %w",
				filePath,
				err,
			)
		}
	}
	return nil
}

func createChartArchive(
	name string,
	version string,
	files map[string]string,
	dir string,
) error {
	chartDir, err := os.MkdirTemp("", "")
	if err != nil {
		return fmt.Errorf(
			"unable to create temp directory for chart %s-%s: %w",
			name,
			version,
			err,
		)
	}
	defer os.RemoveAll(chartDir)
	if err := createFileTree(path.Join(chartDir, name), files); err != nil {
		return fmt.Errorf(
			"unable to create file for chart %s-%s: %w",
			name,
			version,
			err,
		)
	}

	chartArchivePath := path.Join(dir, fmt.Sprintf("%s-%s.tgz", name, version))
	chartArchive, err := os.Create(chartArchivePath)
	if err != nil {
		return fmt.Errorf(
			"unable to create chart archive file %s: %w",
			chartArchivePath,
			err,
		)
	}
	gzipWriter := gzip.NewWriter(chartArchive)
	defer gzipWriter.Close()
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	curDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf(
			"unable to get the current directory: %s",
			err,
		)
	}
	defer os.Chdir(curDir)
	err = os.Chdir(chartDir)
	if err != nil {
		return fmt.Errorf(
			"unable to change directory to %s: %w",
			chartDir,
			err,
		)

	}
	err = filepath.Walk(
		".",
		func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf(
					"unable to open file %s for copying into archive for chart %s-%s: %w",
					path,
					name,
					version,
					err,
				)
			}
			header, err := tar.FileInfoHeader(info, info.Name())
			if err != nil {
				return fmt.Errorf(
					"unable to create tar header for file %s in chart %s-%s: %w",
					path,
					name,
					version,
					err,
				)
			}
			header.Name = path
			if err := tarWriter.WriteHeader(header); err != nil {
				return fmt.Errorf(
					"unable to create tar header for file %s in chart %s-%s: %w",
					path,
					name,
					version,
					err,
				)
			}
			_, err = io.Copy(tarWriter, file)
			if err != nil {
				return fmt.Errorf(
					"unable to write file %s into archive for chart %s-%s: %w",
					path,
					name,
					version,
					err,
				)
			}
			return nil
		},
	)
	return err
}

func indexRepository(dir string, port int) error {
	indexPath := path.Join(dir, "index.yaml")

	repoUrl := fmt.Sprintf("http://localhost:%d", port)
	index, err := repo.IndexDirectory(dir, repoUrl)
	if err != nil {
		return fmt.Errorf(
			"unable to index charts in %s: %w",
			dir,
			err,
		)
	}
	index.SortEntries()
	if err := index.WriteFile(indexPath, 0644); err != nil {
		return fmt.Errorf(
			"unable to write index file %s: %w",
			indexPath,
			err,
		)
	}
	return nil
}

func createSingleChartHelmRepository(
	chartName string,
	chartVersion string,
	files map[string]string,
	port int,
	dir string,
) error {
	err := createChartArchive(chartName, chartVersion, files, dir)
	if err != nil {
		return fmt.Errorf(
			"unable to create chart archive for %s-%s in %s: %w",
			chartName,
			chartVersion,
			dir,
			err,
		)
	}
	if err = indexRepository(dir, port); err != nil {
		return fmt.Errorf(
			"unable to index repository for chart %s-%s in %s: %w",
			chartName,
			chartVersion,
			dir,
			err,
		)
	}
	return nil
}

type logRecord struct {
	Method string
	URL    url.URL
}

type logRecorder struct {
	records []logRecord
}

func serveDirectory(
	dir string,
	logger *slog.Logger,
	recorder *logRecorder,
) (*http.Server, int, <-chan struct{}, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, nil, fmt.Errorf(
			"unable to listen on the loopback interface: %w",
			err,
		)
	}
	handler := http.FileServer(http.Dir(dir))
	if recorder != nil {
		handler = handlers.CustomLoggingHandler(
			ginkgo.GinkgoWriter,
			handlers.LoggingHandler(ginkgo.GinkgoWriter, handler),
			func(_ io.Writer, params handlers.LogFormatterParams) {
				recorder.records = append(recorder.records, logRecord{
					Method: params.Request.Method,
					URL:    params.URL,
				})
			},
		)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{
		Handler: handler,
	}
	done := make(chan struct{})
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			if logger != nil {
				logger.With("error", err, "port", port).Error("unable to serve http")
			}
		}
		close(done)
	}()
	return server, port, done, nil
}

func stopServing(server *http.Server, done <-chan struct{}) error {
	if err := server.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("unable to shut down the server: %w", err)
	}
	<-done
	return nil
}

func getDummySSHCreds(repoURL string) Credentials {
	return Credentials{
		repoURL: RepositoryCreds{
			Credentials: map[string]string{
				"identity":    "dummy",
				"known_hosts": "dummy",
			},
		},
	}
}

type GitClientMock struct {
	mock.Mock
}

func (mock *GitClientMock) Clone(
	ctx context.Context,
	repoURL string,
	config repository.CloneConfig,
) (*git.Commit, error) {
	args := mock.Called(ctx, repoURL, config)
	return args.Get(0).(*git.Commit), args.Error(1)
}

var _ GitClientInterface = &GitClientMock{}

func TestAll(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	format.TruncatedDiff = false
	ginkgo.RunSpecs(t, "Repository Test Suite")
}

var _ = ginkgo.Describe("HelmRelease expansion check", func() {
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

	ginkgo.It("expands HelmRelease from a chart in a Helm repository", func() {
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
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
			"kind: HelmRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			fmt.Sprintf("  url: http://localhost:%d", port),
		}, "\n")
		g.Expect(err).ToNot(gomega.HaveOccurred())

		expander := NewHelmReleaseExpander(ctx, logger, nil)
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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

	ginkgo.It("caches charts in Helm repositories", func() {
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
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
			"kind: HelmRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			fmt.Sprintf("  url: http://localhost:%d", port),
			"---",
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
			"kind: HelmRepository",
			"metadata:",
			"  namespace: testns-different",
			"  name: local-other",
			"spec:",
			fmt.Sprintf("  url: http://localhost:%d", port),
		}, "\n")
		g.Expect(err).ToNot(gomega.HaveOccurred())

		expander := NewHelmReleaseExpander(ctx, logger, nil)
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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

	ginkgo.It("handles multiple objects defined in one chart template", func() {
		repoRoot, err := os.MkdirTemp("", "")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer os.RemoveAll(repoRoot)
		server, port, serverDone, err := serveDirectory(repoRoot, logger, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())

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
				"---",
				"apiVersion: v1",
				"kind: ConfigMap",
				"metadata:",
				"  namespace: {{ .Release.Namespace }}",
				"  name: {{ .Release.Name }}-configmap-2",
				"data: {{- .Values.data | toYaml | nindent 2 }}",
			}, "\n"),
		}

		err = createSingleChartHelmRepository(
			"test-chart",
			"0.1.0",
			chartFiles,
			port,
			repoRoot,
		)
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
			"kind: HelmRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			fmt.Sprintf("  url: http://localhost:%d", port),
		}, "\n")
		g.Expect(err).ToNot(gomega.HaveOccurred())

		expander := NewHelmReleaseExpander(ctx, logger, nil)
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v1",
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test-configmap-2",
			"data:",
			"  foo: baz",
			"",
		}, "\n"),
		))
	})

	ginkgo.It("expands HelmRelease from a chart in a Git repository", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
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
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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

	ginkgo.It("caches charts in Git repositories", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
			"kind: GitRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			"  url: " + repoURL,
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
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
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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

	ginkgo.It("handles releative dependency chart paths in a Git repository", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
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
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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

	ginkgo.It("honors dependency chart conditions", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"      enabled: false",
			"      data:",
			"        foo: bar",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
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
				"  condition: dependency-chart.enabled",
			}, "\n"),
			"test-chart/values.yaml": strings.Join([]string{
				"data:",
				"  foo: bar",
				"dependency-chart:",
				"  enabled: false",
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
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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
			"", // Templates from the disabled dependency charts do not show up.
		}, "\n"),
		))
	})

	ginkgo.It("assigns namespace to generated objects if not present", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"      enabled: false",
			"      data:",
			"        foo: bar",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
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
			}, "\n"),
			"test-chart/values.yaml": strings.Join([]string{
				"data:",
				"  foo: bar",
			}, "\n"),
			"test-chart/templates/serviceaccount.yaml": strings.Join([]string{
				"apiVersion: v1",
				"kind: ServiceAccount",
				"metadata:",
				"  name: {{ .Release.Name }}-serviceaccount",
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
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			false,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(output.String()).To(gomega.Equal(strings.Join([]string{
			input,
			"---",
			"# Source: test-chart/templates/serviceaccount.yaml",
			"apiVersion: v1",
			"kind: ServiceAccount",
			"metadata:",
			"  name: testns-test-serviceaccount",
			"  namespace: testns", // Namespace is added as the last metadata attribute.
			"",
		}, "\n"),
		))
	})

	ginkgo.It("passes specified Kubernetes version", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"      enabled: false",
			"      data:",
			"        foo: bar",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
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
				"data:",
				"  kube-version: {{ .Capabilities.KubeVersion.Version }}",
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
		)
		kubeVersion, err := chartutil.ParseKubeVersion("1.222")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			kubeVersion,
			nil,
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
			"  kube-version: v1.222.0",
			"",
		}, "\n"),
		))
	})

	ginkgo.It("passes specified API versions to charts", func() {
		var repoRoot string
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"      enabled: false",
			"      data:",
			"        foo: bar",
			"---",
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
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
			}, "\n"),
			"test-chart/values.yaml": strings.Join([]string{
				"data:",
				"  foo: bar",
			}, "\n"),
			"test-chart/templates/configmap.yaml": strings.Join([]string{
				`apiVersion: {{ .Capabilities.APIVersions.Has "v2" | ternary "v2" "v1" }}`,
				"kind: ConfigMap",
				"metadata:",
				"  namespace: {{ .Release.Namespace }}",
				"  name: {{ .Release.Name }}-configmap",
				"data:",
				`  keeps-default-capabilities: {{ .Capabilities.APIVersions.Has "policy/v1" }}`,
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
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			[]string{"v2"},
			false,
		)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(output.String()).To(gomega.Equal(strings.Join([]string{
			input,
			"---",
			"# Source: test-chart/templates/configmap.yaml",
			"apiVersion: v2", // The chart generates v2 as API version as it's available in capabilities.
			"kind: ConfigMap",
			"metadata:",
			"  namespace: testns",
			"  name: testns-test-configmap",
			"data:",
			"  keeps-default-capabilities: true", // The chart also has access to default capabilities.
			"",
		}, "\n"),
		))
	})

	ginkgo.It("substitutes HTTPS repository URL when configured with username/password credential", func() {
		var repoRoot string
		sshURL := "ssh://git@localhost/dummy.git"
		httpsURL := "https://localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
			"kind: GitRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			"  url: " + sshURL,
		}, "\n")

		chartFiles := map[string]string{
			"test-chart/Chart.yaml": strings.Join([]string{
				"apiVersion: v2",
				"name: test-chart",
				"version: 0.1.0",
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
				"data:",
				"  foo: bar",
			}, "\n"),
		}

		gitClient := &GitClientMock{}
		gitClient.
			// Now connects to the HTTPS URL rather than the SSH one.
			On("Clone", mock.Anything, httpsURL, mock.Anything).
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
		)
		credentials := Credentials{
			sshURL: RepositoryCreds{
				Credentials: map[string]string{
					"username": "dummy",
					"password": "dummy",
				},
			},
		}
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			credentials,
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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
			"  foo: bar",
			"",
		}, "\n"),
		))
	})

	ginkgo.It("reports error when required credentials are missing", func() {
		repoURL := "ssh://git@localhost/dummy.git"
		input := strings.Join([]string{
			"apiVersion: helm.toolkit.fluxcd.io/v2beta2",
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
			"apiVersion: source.toolkit.fluxcd.io/v1beta2",
			"kind: GitRepository",
			"metadata:",
			"  namespace: testns",
			"  name: local",
			"spec:",
			"  url: " + repoURL,
		}, "\n")

		gitClient := &GitClientMock{}
		gitClient.
			// Now connects to the HTTPS URL rather than the SSH one.
			On("Clone", mock.Anything, repoURL, mock.Anything).
			Return(nil, fmt.Errorf("authentication required"))
		expander := NewHelmReleaseExpander(
			ctx,
			logger,
			func(
				path string,
				authOpts *git.AuthOptions,
				clientOpts ...gogit.ClientOption,
			) (GitClientInterface, error) {
				return gitClient, nil
			},
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			Credentials{}, // No credentials provided.
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			false,
		)
		g.Expect(err).To(gomega.MatchError(
			gomega.ContainSubstring("'identity' is required")),
		)
	})
})

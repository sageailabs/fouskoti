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
	"time"

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
				"unable to create directory for test file %s: %w",
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

type fsInfo struct {
	name string
	mode int64
	size int64
	time time.Time
}

func (fileInfo fsInfo) Name() string {
	return fileInfo.name
}

func (fileInfo fsInfo) Size() int64 {
	return fileInfo.size
}

func (fileInfo fsInfo) Mode() fs.FileMode {
	return os.FileMode(fileInfo.mode)
}

func (fileInfo fsInfo) ModTime() time.Time {
	return fileInfo.time
}

func (fileInfo fsInfo) IsDir() bool {
	return fileInfo.Mode().IsDir()
}

func (fileInfo fsInfo) Sys() any {
	return nil
}

var _ os.FileInfo = fsInfo{}

func getFileInfo(name string, content string) os.FileInfo {
	return fsInfo{
		name: name,
		mode: 0,
		size: int64(len(content)),
		time: time.Now(),
	}
}

func createChartArchive(
	name string,
	version string,
	files map[string]string,
) (*bytes.Buffer, error) {
	chartArchive := &bytes.Buffer{}
	gzipWriter := gzip.NewWriter(chartArchive)
	defer gzipWriter.Close()
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	for path, content := range files {
		info := getFileInfo(filepath.Join(name, path), content)
		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return nil, fmt.Errorf(
				"unable to create tar header for file %s in chart %s-%s: %w",
				path,
				name,
				version,
				err,
			)
		}
		header.Name = filepath.Join(name, path)
		if err := tarWriter.WriteHeader(header); err != nil {
			return nil, fmt.Errorf(
				"unable to create tar header for file %s in chart %s-%s: %w",
				path,
				name,
				version,
				err,
			)
		}
		_, err = io.Copy(tarWriter, bytes.NewBufferString(content))
		if err != nil {
			return nil, fmt.Errorf(
				"unable to write file %s into archive for chart %s-%s: %w",
				path,
				name,
				version,
				err,
			)
		}
	}

	return chartArchive, nil
}

func createChartArchiveInDir(
	name string,
	version string,
	files map[string]string,
	dir string,
) error {
	buffer, err := createChartArchive(name, version, files)
	if err != nil {
		return fmt.Errorf("unable to create chart archive: %w", err)
	}
	chartArchivePath := path.Join(dir, fmt.Sprintf("%s-%s.tgz", name, version))
	chartArchiveFile, err := os.Create(chartArchivePath)
	if err != nil {
		return fmt.Errorf(
			"unable to create chart archive file %s: %w",
			chartArchivePath,
			err,
		)
	}
	_, err = chartArchiveFile.Write(buffer.Bytes())
	if err != nil {
		return fmt.Errorf(
			"unable to write chart archive file %s: %w",
			chartArchivePath,
			err,
		)
	}
	err = chartArchiveFile.Close()
	if err != nil {
		return fmt.Errorf(
			"unable to save chart archive file %s: %w",
			chartArchivePath,
			err,
		)
	}
	return nil
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
	err := createChartArchiveInDir(chartName, chartVersion, files, dir)
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

	ginkgo.BeforeEach(func() {
		g = gomega.NewWithT(ginkgo.GinkgoT())
		ctx = context.Background()
		handler := slog.NewTextHandler(
			ginkgo.GinkgoWriter,
			&slog.HandlerOptions{AddSource: true, Level: slog.LevelDebug},
		)
		logger = slog.New(handler)
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
		g.Expect(err).ToNot(gomega.HaveOccurred())

		expander := NewHelmReleaseExpander(ctx, logger, nil, nil)
		output := &bytes.Buffer{}
		err = expander.ExpandHelmReleases(
			Credentials{},
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
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

	ginkgo.It("honors dependency chart conditions", func() {
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
			"      enabled: false",
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
			"", // Templates from the disabled dependency charts do not show up.
		}, "\n"),
		))
	})

	ginkgo.It("assigns namespace to generated objects if not present", func() {
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
			"      enabled: false",
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
			"      enabled: false",
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
			nil,
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
			"  kube-version: v1.222.0",
			"",
		}, "\n"),
		))
	})

	ginkgo.It("passes specified API versions to charts", func() {
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
			"      enabled: false",
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
			nil,
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			getDummySSHCreds(repoURL),
			bytes.NewBufferString(input),
			output,
			nil,
			[]string{"v2"},
			"",
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
			nil,
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
			"  foo: bar",
			"",
		}, "\n"),
		))
	})

	ginkgo.It("reports error when required credentials are missing", func() {
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
			nil,
		)
		output := &bytes.Buffer{}
		err := expander.ExpandHelmReleases(
			Credentials{}, // No credentials provided.
			bytes.NewBufferString(input),
			output,
			nil,
			nil,
			"",
			false,
		)
		g.Expect(err).To(gomega.MatchError(
			gomega.ContainSubstring("'identity' is required")),
		)
	})
})

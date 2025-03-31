// Copyright © The Sage Group plc or its licensors.

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/git"
	"github.com/fluxcd/pkg/git/gogit"
	"github.com/fluxcd/pkg/git/repository"
	"helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/kustomize/api/filters/namespace"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	yamlutil "github.com/sageailabs/fouskoti/pkg/yaml"
)

type chartContext struct {
	localRepoPath string
	chartName     string
	loader        repositoryLoader
	repoNode      *yaml.RNode
}

type repositoryLoader interface {
	// loadRepositoryChart loads a chart from repository with a URL specified
	// either in repoURL or in repoNode.
	loadRepositoryChart(
		repoNode *yaml.RNode,
		repoURL string,
		parentContext *chartContext,
		chartName string,
		chartVersion string,
	) (*chart.Chart, error)
}

type GitClientInterface interface {
	Clone(
		ctx context.Context,
		repoURL string,
		config repository.CloneConfig,
	) (*git.Commit, error)
}

type gitClientFactoryFunc func(
	path string,
	authOpts *git.AuthOptions,
	clientOpts ...gogit.ClientOption,
) (GitClientInterface, error)

type loaderConfig struct {
	ctx               context.Context
	logger            *slog.Logger
	gitClientFactory  gitClientFactoryFunc
	repoClientFactory repositoryClientFactoryFunc
	cacheRoot         string
	chartCache        map[string]*chart.Chart
	credentials       Credentials
}

type repositoryLoaderFactory func(config loaderConfig) repositoryLoader

func getRepoFactory(
	repoNode *yaml.RNode,
) (repositoryLoaderFactory, error) {
	switch repoNode.GetKind() {
	case "HelmRepository":
		repoTypeIf, err := repoNode.GetFieldValue("spec.type")
		if errors.Is(err, yaml.NoFieldError{Field: "spec.type"}) {
			return newHelmRepositoryLoader, nil
		}
		if err != nil {
			return nil, fmt.Errorf(
				"error retrieving spec.type for %s %s/%s: %v",
				repoNode.GetKind(),
				repoNode.GetNamespace(),
				repoNode.GetName(),
				err,
			)
		}
		repoType, ok := repoTypeIf.(string)
		if !ok {
			return nil, fmt.Errorf(
				"invalid value for spec.type for %s %s/%s: %v",
				repoNode.GetKind(),
				repoNode.GetNamespace(),
				repoNode.GetName(),
				repoTypeIf,
			)
		}
		if repoType != "oci" {
			return newHelmRepositoryLoader, nil
		}
		return newOciRepositoryLoader, nil
	case "GitRepository":
		return newGitRepositoryLoader, nil
	case "OCIRepository":
		return newOciRepositoryLoader, nil
	default:
		return nil, fmt.Errorf(
			"unknown kind %s for repository %s/%s",
			repoNode.GetKind(),
			repoNode.GetNamespace(),
			repoNode.GetName(),
		)
	}
}

func getRepoFactoryByURL(repoURL string) (repositoryLoaderFactory, error) {
	var result repositoryLoaderFactory

	parsedURL, err := url.Parse(repoURL)
	if err != nil {
		return nil, fmt.Errorf("unable to parse chart repository URL %s", err)
	}

	switch parsedURL.Scheme {
	case "https", "http":
		if parsedURL.User.Username() == "git" {
			result = newGitRepositoryLoader
		} else {
			result = newHelmRepositoryLoader
		}
	case "ssh":
		result = newGitRepositoryLoader
	case "oci":
		result = newOciRepositoryLoader
	default:
		return nil, fmt.Errorf("unknown type for repository URL %s", repoURL)
	}
	return result, nil
}

func getLoaderForRepo(
	repoNode *yaml.RNode,
	config loaderConfig,
) (repositoryLoader, error) {
	factory, err := getRepoFactory(repoNode)
	if err != nil {
		return nil, err
	}

	return factory(config), nil
}

func getLoaderForRepoURL(
	repoURL string,
	config loaderConfig,
) (repositoryLoader, error) {
	factory, err := getRepoFactoryByURL(repoURL)
	if err != nil {
		return nil, err
	}

	return factory(config), nil
}

func joinPath(a string, b string) string {
	if path.IsAbs(b) {
		return b
	}
	return path.Join(a, b)
}

func decodeToObject(node *yaml.RNode, out runtime.Object) error {
	bytes, err := node.MarshalJSON()
	if err != nil {
		return fmt.Errorf("unable to encode node to JSON: %w", err)
	}
	err = json.Unmarshal(bytes, out)
	if err != nil {
		return fmt.Errorf("unable to unmarshal JSON to k8s object: %w", err)
	}
	return nil
}

func getCachePathForRepo(cacheRoot string, repoURL string, ephemeral bool) string {
	urlPath := strings.ReplaceAll(strings.TrimSuffix(repoURL, "/"), "/", "#")
	parts := []string{cacheRoot}
	if ephemeral {
		parts = append(parts, "ephemeral")
	}
	parts = append(parts, urlPath)
	return path.Join(parts...)
}

func saveChartFiles(files []*helmloader.BufferedFile, chartDir string) error {
	for _, file := range files {
		filePath := path.Join(chartDir, file.Name)
		fileDir := path.Dir(filePath)
		err := os.MkdirAll(fileDir, 0700)
		if err != nil {
			return fmt.Errorf("unable to create chart cache directory %s: %w", fileDir, err)
		}
		err = os.WriteFile(filePath, file.Data, 0660)
		if err != nil {
			return fmt.Errorf("unable to write cached chart file %s: %w", filePath, err)
		}
	}
	return nil
}

// loadRepositoryChart downloads the chart and returns it.
func loadRepositoryChart(
	ctx context.Context,
	logger *slog.Logger,
	gitClientFactory gitClientFactoryFunc,
	repoClientFactory repositoryClientFactoryFunc,
	chartCacheDir string,
	chartCache map[string]*chart.Chart,
	credentials Credentials,
	release *helmv2.HelmRelease,
	repoNode *yaml.RNode,
) (*chart.Chart, error) {
	if chartCacheDir == "" {
		var err error
		chartCacheDir, err = os.MkdirTemp("", "chart-repo-cache-")
		if err != nil {
			return nil, fmt.Errorf(
				"unable to create a cache dir for repo %s/%s/%s: %w",
				repoNode.GetKind(),
				repoNode.GetNamespace(),
				repoNode.GetName(),
				err,
			)
		}
		defer os.RemoveAll(chartCacheDir)
	}

	loader, err := getLoaderForRepo(
		repoNode,
		loaderConfig{
			ctx,
			logger,
			gitClientFactory,
			repoClientFactory,
			chartCacheDir,
			chartCache,
			credentials,
		},
	)
	if err != nil {
		return nil, err
	}

	return loader.loadRepositoryChart(
		repoNode,
		"",
		nil,
		release.Spec.Chart.Spec.Chart,
		release.Spec.Chart.Spec.Version,
	)
}

func loadChartDependencies(
	config loaderConfig,
	parentChart *chart.Chart,
	parentContext *chartContext,
) error {
	for _, dependency := range parentChart.Metadata.Dependencies {
		if dependency.Repository == "" {
			// This is a bundled chart, and those do not have repository
			// information and are not addressable outside of the parent chart.
			continue
		}
		repoURL, err := normalizeURL(dependency.Repository)
		if err != nil {
			return fmt.Errorf(
				"unable to normalize URL for dependency chart %s/%s: %w",
				dependency.Name,
				dependency.Version,
				err,
			)
		}

		parsedURL, _ := url.Parse(repoURL)
		if parsedURL.Host == ".." {
			parsedURL.Host = ""
			parsedURL.Path = path.Join("..", parsedURL.Path)
		}
		var dependencyChart *chart.Chart
		switch parsedURL.Scheme {
		case "file", "":
			dependencyChart, err = parentContext.loader.loadRepositoryChart(
				parentContext.repoNode,
				"",
				parentContext,
				joinPath(parentContext.chartName, parsedURL.Path),
				dependency.Version,
			)
		default:
			var loader repositoryLoader
			loader, err = getLoaderForRepoURL(repoURL, config)
			if err != nil {
				return fmt.Errorf(
					"unable to get loader for chart %s/%s in %s (a dependency of %s): %w",
					dependency.Name,
					dependency.Version,
					repoURL,
					parentChart.Name(),
					err,
				)
			}

			dependencyChart, err = loader.loadRepositoryChart(
				nil,
				repoURL,
				nil,
				dependency.Name,
				dependency.Version,
			)
		}
		if err != nil {
			return fmt.Errorf(
				"unable to load chart %s/%s from %s (a dependency of %s): %w",
				dependency.Name,
				dependency.Version,
				repoURL,
				parentChart.Name(),
				err,
			)
		}
		parentChart.AddDependency(dependencyChart)
	}
	return nil
}

func expandHelmRelease(
	ctx context.Context,
	logger *slog.Logger,
	gitClientFactory gitClientFactoryFunc,
	repoClientFactory repositoryClientFactoryFunc,
	kubeVersion *chartutil.KubeVersion,
	apiVersions []string,
	chartCacheDir string,
	chartCache map[string]*chart.Chart,
	credentials Credentials,
	releaseNode *yaml.RNode,
	repoNode *yaml.RNode,
) ([]*yaml.RNode, error) {
	var release helmv2.HelmRelease
	err := decodeToObject(releaseNode, &release)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to decode HelmRelease: %w",
			err,
		)
	}

	if repoNode == nil {
		return nil, fmt.Errorf(
			"missing chart repository for Helm release %s/%s",
			release.Namespace,
			release.Name,
		)
	}

	chart, err := loadRepositoryChart(
		ctx,
		logger,
		gitClientFactory,
		repoClientFactory,
		chartCacheDir,
		chartCache,
		credentials,
		&release,
		repoNode,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to load chart for %s %s/%s: %w",
			repoNode.GetKind(),
			repoNode.GetNamespace(),
			repoNode.GetName(),
			err,
		)
	}

	// Remove charts disabled by conditions.
	err = chartutil.ProcessDependenciesWithMerge(chart, release.GetValues())
	if err != nil {
		return nil, fmt.Errorf(
			"unable to process dependencies for chart %s: %w",
			chart.Name(),
			err,
		)
	}

	values, err := chartutil.CoalesceValues(chart, release.GetValues())
	if err != nil {
		return nil, fmt.Errorf(
			"unable to coalesce values from the chart for release %s/%s: %w",
			release.Namespace,
			release.Name,
			err,
		)
	}

	capabilities := chartutil.DefaultCapabilities.Copy()
	if kubeVersion != nil {
		capabilities.KubeVersion = *kubeVersion
	}
	if len(apiVersions) > 0 {
		capabilities.APIVersions = append(
			capabilities.APIVersions,
			chartutil.VersionSet(apiVersions)...,
		)
	}

	targetNamespace := release.Spec.TargetNamespace
	if targetNamespace == "" {
		targetNamespace = release.Namespace
	}
	releaseName := release.Spec.ReleaseName
	if releaseName == "" {
		releaseName = fmt.Sprintf("%s-%s", targetNamespace, release.Name)
	}

	options := chartutil.ReleaseOptions{
		Name:      releaseName,
		Namespace: targetNamespace,
		Revision:  1,
		IsInstall: true,
		IsUpgrade: false,
	}
	valuesToRender, err := chartutil.ToRenderValues(chart, values, options, capabilities)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to compose values to render Helm release %s/%s: %w",
			release.Namespace,
			release.Name,
			err,
		)
	}
	manifests, err := engine.Render(chart, valuesToRender)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to render values for Helm release %s/%s: %w",
			release.Namespace,
			release.Name,
			err,
		)
	}

	var results []*yaml.RNode
	for key, manifest := range manifests {
		if strings.TrimSpace(manifest) == "" {
			continue
		}
		if filepath.Base(key) == "NOTES.txt" {
			continue
		}
		reader := kio.ByteReader{
			Reader: bytes.NewBufferString(manifest),
		}
		result, err := reader.Read()
		if err != nil {
			return nil, fmt.Errorf(
				"unable to parse manifest %s from Helm release %s/%s: %w",
				key,
				release.Namespace,
				release.Name,
				err,
			)
		}
		for _, node := range result {
			node.YNode().HeadComment = fmt.Sprintf("Source: %s", key)
			results = append(results, node)
		}
	}

	filter := &namespace.Filter{
		Namespace:              release.Namespace,
		UnsetOnly:              true,
		SetRoleBindingSubjects: namespace.NoSubjects,
	}
	results, err = filter.Filter(results)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to assign namespace to resources generated from %s %s/%s: %w",
			release.Kind,
			release.Namespace,
			release.Name,
			err,
		)
	}
	return results, nil
}

func getRepositoryForHelmRelease(
	nodes []*yaml.RNode,
	helmRelease *yaml.RNode,
) (*yaml.RNode, error) {
	repoKind, err := helmRelease.GetString("spec.chart.spec.sourceRef.kind")
	if err != nil {
		return nil, fmt.Errorf("unable to get kind for the repository: %w", err)
	}

	repoName, err := helmRelease.GetString("spec.chart.spec.sourceRef.name")
	if err != nil {
		return nil, fmt.Errorf("unable to get name for the repository: %w", err)
	}

	repoNamespace, err := yamlutil.GetStringOr(
		helmRelease,
		"spec.chart.spec.sourceRef.namespace",
		helmRelease.GetNamespace(),
	)
	if err != nil {
		return nil, err
	}

	repoApiVersion, err := yamlutil.GetStringOr(
		helmRelease,
		"spec.chart.spec.sourceRef.apiVersion",
		"",
	)
	if err != nil {
		return nil, err
	}

	for _, node := range nodes {
		if node.GetKind() == repoKind &&
			node.GetName() == repoName &&
			node.GetNamespace() == repoNamespace &&
			(repoApiVersion == "" || node.GetApiVersion() == repoApiVersion) {
			return node, nil
		}
	}
	return nil, nil
}

type releaseRepo struct {
	release *yaml.RNode
	repo    *yaml.RNode
}

func getReleaseRepos(
	repoNodes []*yaml.RNode,
	releaseNodes []*yaml.RNode,
) ([]releaseRepo, error) {
	result := []releaseRepo{}
	helmReleases := []*yaml.RNode{}

	for _, node := range releaseNodes {
		if yamlutil.GetGroup(node) == "helm.toolkit.fluxcd.io" &&
			node.GetKind() == "HelmRelease" {
			helmReleases = append(helmReleases, node)
		}
	}

	for _, helmRelease := range helmReleases {
		repository, err := getRepositoryForHelmRelease(repoNodes, helmRelease)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to find repository for HelmRelease %s/%s: %w",
				helmRelease.GetNamespace(),
				helmRelease.GetName(),
				err)
		}
		result = append(result, releaseRepo{release: helmRelease, repo: repository})
	}
	return result, nil
}

type releaseRepoRenderer struct {
	ctx               context.Context
	logger            *slog.Logger
	gitClientFactory  gitClientFactoryFunc
	repoClientFactory repositoryClientFactoryFunc
	kubeVersion       *chartutil.KubeVersion
	apiVersions       []string
	maxExpansions     int
	chartCacheDir     string
	chartCache        map[string]*chart.Chart
	credentials       Credentials
}

func newReleaseRepoRenderer(
	ctx context.Context,
	logger *slog.Logger,
	gitClientFactory gitClientFactoryFunc,
	repoClientFactory repositoryClientFactoryFunc,
	kubeVersion *chartutil.KubeVersion,
	apiVersions []string,
	maxExpansions int,
	chartCacheDir string,
	chartCache map[string]*chart.Chart,
	credentials Credentials,
) *releaseRepoRenderer {
	return &releaseRepoRenderer{
		ctx:               ctx,
		logger:            logger,
		gitClientFactory:  gitClientFactory,
		repoClientFactory: repoClientFactory,
		kubeVersion:       kubeVersion,
		apiVersions:       apiVersions,
		maxExpansions:     maxExpansions,
		chartCacheDir:     chartCacheDir,
		chartCache:        chartCache,
		credentials:       credentials,
	}
}

func (renderer *releaseRepoRenderer) filterStep(
	allNodes []*yaml.RNode,
	nodesToRender []*yaml.RNode,
) ([]*yaml.RNode, []*yaml.RNode, error) {
	result := []*yaml.RNode{}

	releaseRepos, err := getReleaseRepos(allNodes, nodesToRender)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get release repos: %w", err)
	}

	for _, pair := range releaseRepos {
		expanded, err := expandHelmRelease(
			renderer.ctx,
			renderer.logger,
			renderer.gitClientFactory,
			renderer.repoClientFactory,
			renderer.kubeVersion,
			renderer.apiVersions,
			renderer.chartCacheDir,
			renderer.chartCache,
			renderer.credentials,
			pair.release,
			pair.repo,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"unable to expand Helm release %s/%s: %w",
				pair.release.GetNamespace(),
				pair.release.GetName(),
				err,
			)
		}
		result = append(result, expanded...)
	}

	slices.SortStableFunc(result, func(a, b *yaml.RNode) int {
		aKind := a.GetKind()
		bKind := b.GetKind()
		if aKind < bKind {
			return -1
		} else if aKind > bKind {
			return 1
		}

		aVersion := a.GetApiVersion()
		bVersion := b.GetApiVersion()
		if aVersion < bVersion {
			return -1
		} else if aVersion > bVersion {
			return 1
		}

		aNamespace := a.GetNamespace()
		bNamespace := b.GetNamespace()
		if aNamespace < bNamespace {
			return -1
		} else if aNamespace > bNamespace {
			return 1
		}

		aName := a.GetName()
		bName := b.GetName()
		if aName < bName {
			return -1
		} else if aName > bName {
			return 1
		}
		return 0
	})
	return append(allNodes, result...), result, nil
}

func (renderer *releaseRepoRenderer) Filter(
	nodes []*yaml.RNode,
) ([]*yaml.RNode, error) {
	newNodes := nodes
	for range renderer.maxExpansions {
		var err error
		nodes, newNodes, err = renderer.filterStep(nodes, newNodes)
		if err != nil {
			return nil, err
		}
		if len(newNodes) == 0 {
			break
		}
	}
	return nodes, nil
}

type HelmReleaseExpander struct {
	ctx               context.Context
	logger            *slog.Logger
	gitClientFactory  gitClientFactoryFunc
	repoClientFactory repositoryClientFactoryFunc
}

func NewHelmReleaseExpander(
	ctx context.Context,
	logger *slog.Logger,
	gitClientFactory gitClientFactoryFunc,
	repoClientFactory repositoryClientFactoryFunc,
) *HelmReleaseExpander {
	return &HelmReleaseExpander{
		ctx:               ctx,
		logger:            logger,
		gitClientFactory:  gitClientFactory,
		repoClientFactory: repoClientFactory,
	}
}

func (expander *HelmReleaseExpander) ExpandHelmReleases(
	credentials Credentials,
	input io.Reader,
	output io.Writer,
	kubeVersion *chartutil.KubeVersion,
	apiVersions []string,
	maxExpansions int,
	chartCacheDir string,
	enableChartInMemoryCache bool,
) error {
	var chartCache map[string]*chart.Chart
	if enableChartInMemoryCache {
		chartCache = make(map[string]*chart.Chart)
	}

	// Non-fixed GitRepository references like branches are not cacheable and
	// are left in the ephemeral subtree, which we need to clean up at the
	// end.
	defer func() {
		if chartCacheDir != "" {
			ephemeralCacheDir := filepath.Join(chartCacheDir, "ephemeral")
			if err := os.RemoveAll(ephemeralCacheDir); err != nil {
				expander.logger.
					With("directory", ephemeralCacheDir).
					With("error", err).
					Error("Unable to clean up ephemeral repository directory")
			}
		}
	}()

	filter := newReleaseRepoRenderer(
		expander.ctx,
		expander.logger,
		expander.gitClientFactory,
		expander.repoClientFactory,
		kubeVersion,
		apiVersions,
		maxExpansions,
		chartCacheDir,
		chartCache,
		credentials,
	)

	return kio.Pipeline{
		Inputs:  []kio.Reader{&kio.ByteReader{Reader: input}},
		Filters: []kio.Filter{filter},
		Outputs: []kio.Writer{kio.ByteWriter{Writer: output}},
	}.Execute()
}

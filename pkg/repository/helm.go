// Copyright © The Sage Group plc or its licensors.

package repository

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"helm.sh/helm/v4/pkg/chart/loader/archive"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	helmloader "helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	helmgetter "helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/helmpath"
	"helm.sh/helm/v4/pkg/registry"
	helmrepo "helm.sh/helm/v4/pkg/repo/v1"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// normalizeURL normalizes a ChartRepository URL by its scheme.
func normalizeURL(repositoryURL string) (string, error) {
	if repositoryURL == "" {
		return "", nil
	}
	u, err := url.Parse(repositoryURL)
	if err != nil {
		return "", err
	}

	if u.Scheme == registry.OCIScheme {
		u.Path = strings.TrimRight(u.Path, "/")
		// we perform the same operation on u.RawPath so that it will be a valid encoding
		// of u.Path. This allows u.EscapedPath() (which is used in computing u.String()) to return
		// the correct value when the path is url encoded.
		// ref: https://pkg.go.dev/net/url#URL.EscapedPath
		u.RawPath = strings.TrimRight(u.RawPath, "/")
		return u.String(), nil
	}

	u.Path = strings.TrimRight(u.Path, "/") + "/"
	u.RawPath = strings.TrimRight(u.RawPath, "/") + "/"
	return u.String(), nil
}

type helmRepoChartLoader struct {
	loaderConfig
}

func newHelmRepositoryLoader(config loaderConfig) repositoryLoader {
	return &helmRepoChartLoader{loaderConfig: config}
}

func (loader *helmRepoChartLoader) loadRepositoryChart(
	repoNode *yaml.RNode,
	repoURL string,
	parentContext *chartContext,
	chartName string,
	chartVersionSpec string,
) (*chart.Chart, error) {
	start := time.Now()
	savedLogger := loader.logger
	defer func() { loader.logger = savedLogger }()

	if repoNode != nil {
		var repo sourcev1.HelmRepository
		err := decodeToObject(repoNode, &repo)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to decode HelmRepository %s/%s: %w",
				repoNode.GetNamespace(),
				repoNode.GetName(),
				err,
			)
		}
		loader.logger = loader.logger.With(
			"namespace", repo.Namespace,
			"name", repo.Name,
		)
		repoURL = repo.Spec.URL
	}

	repoURL, err := normalizeURL(repoURL)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid Helm repository URL %s: %w",
			repoURL,
			err,
		)
	}

	loader.logger = loader.logger.With(
		"url", repoURL,
		"chart", chartName,
		"version", chartVersionSpec,
	)
	loader.logger.Debug("Loading chart from Helm repository")

	repoPath := getCachePathForRepo(loader.cacheRoot, repoURL, false)
	getters := helmgetter.All(&cli.EnvSettings{})
	chartRepo, err := helmrepo.NewChartRepository(
		&helmrepo.Entry{
			Name: "repo",
			URL:  repoURL,
			// TODO(vlad): Use chart repository options when provided.
		},
		getters,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create chart repository object: %w", err)
	}

	chartRepo.CachePath = repoPath

	indexFilePath := filepath.Join(
		repoPath,
		helmpath.CacheIndexFile(chartRepo.Config.Name),
	)
	if _, err := os.Stat(indexFilePath); os.IsNotExist(err) {
		indexFilePath, err = chartRepo.DownloadIndexFile()
		if err != nil {
			return nil, fmt.Errorf(
				"unable to download index file for Helm repository %s: %w",
				repoURL,
				err,
			)
		}
	}
	repoIndex, err := helmrepo.LoadIndexFile(indexFilePath)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to load index file for Helm repository %s: %w",
			repoURL,
			err,
		)
	}
	chartRepo.IndexFile = repoIndex
	version, err := repoIndex.Get(chartName, chartVersionSpec)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to get chart %s/%s from Helm repository %s: %w",
			chartName,
			chartVersionSpec,
			repoURL,
			err,
		)
	}

	chartVersion := version.Version
	chartKey := fmt.Sprintf("%s#%s#%s", repoURL, chartName, chartVersion)
	if loader.chartCache != nil {
		if chart, ok := loader.chartCache[chartKey]; ok {
			loader.logger.Debug("Using chart from in-memory cache")
			return chart, nil
		}
	}

	chartDir := filepath.Join(
		chartRepo.CachePath,
		fmt.Sprintf("%s-%s", chartName, chartVersion),
	)
	var chart *chart.Chart
	var stat os.FileInfo
	if stat, err = os.Stat(chartDir); err == nil && stat.IsDir() {
		chart, err = helmloader.LoadDir(chartDir)
	}

	if err != nil {
		if err := os.RemoveAll(chartDir); err != nil {
			loader.logger.
				With("error", err).
				With("dir", chartDir).
				Error("Unable to remove the chart cache directory")
		}

		parsedURL, err := url.Parse(version.URLs[0])
		if err != nil {
			return nil, fmt.Errorf(
				"unable to parse chart URL %s: %w",
				version.URLs[0],
				err,
			)
		}
		if parsedURL.Host == "" && !path.IsAbs(parsedURL.Path) {
			// Adjust the URL to be absolute.
			parsedRepoURL, _ := url.Parse(repoURL)
			parsedRepoURL.Path = path.Join(parsedRepoURL.Path, parsedURL.Path)
			parsedURL = parsedRepoURL
		}

		getter, err := getters.ByScheme(parsedURL.Scheme)
		if err != nil {
			return nil, fmt.Errorf(
				"unknown scheme %s for chart %s: %w",
				parsedURL.Scheme,
				version.URLs[0],
				err,
			)
		}

		chartData, err := getter.Get(
			parsedURL.String(),
			[]helmgetter.Option{}...) // TODO(vlad): Set options if necessary.
		if err != nil {
			return nil, fmt.Errorf(
				"unable to download chart %s: %w",
				parsedURL.String(),
				err,
			)
		}

		files, err := archive.LoadArchiveFiles(chartData)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to load chart archive %s/%s in %s: %w",
				chartName,
				chartVersionSpec,
				repoURL,
				err,
			)
		}

		chart, err = helmloader.LoadFiles(files)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to load chart files %s/%s in %s: %w",
				chartName,
				chartVersionSpec,
				repoURL,
				err,
			)
		}

		err = saveChartFiles(files, chartDir)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to save chart files %s/%s in %s to cache: %w",
				chartName,
				chartVersionSpec,
				repoURL,
				err,
			)
		}
	} else {
		loader.logger.Debug("Using cached Helm chart")
	}

	startDeps := time.Now()
	loader.logger = loader.logger.WithGroup("deps")
	err = loadChartDependencies(loader.loaderConfig, chart, nil)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to load chart dependencies for %s/%s in %s: %w",
			chartName,
			chart.Metadata.Version,
			repoURL,
			err,
		)
	}
	loader.logger.
		With("duration", time.Since(startDeps)).
		Debug("Finished loading deps")

	if loader.chartCache != nil {
		loader.chartCache[chartKey] = chart
	}

	loader.logger.
		With("version", chart.Metadata.Version).
		With("duration", time.Since(start)).
		Debug("Finished loading chart")
	return chart, nil
}

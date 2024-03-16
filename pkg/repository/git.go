package repository

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"time"

	"github.com/fluxcd/pkg/git"
	"github.com/fluxcd/pkg/git/gogit"
	"github.com/fluxcd/pkg/git/repository"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

type gitRepoChartLoader struct {
	loaderConfig
}

func newGitRepositoryLoader(config loaderConfig) repositoryLoader {
	return &gitRepoChartLoader{loaderConfig: config}
}

func normalizeGitReference(
	original *sourcev1.GitRepositoryRef,
) *sourcev1.GitRepositoryRef {
	if original != nil &&
		(original.Branch == "" ||
			original.Tag == "" ||
			original.SemVer == "" ||
			original.Name == "" ||
			original.Commit == "") {
		return original
	}
	return &sourcev1.GitRepositoryRef{Branch: "master"}
}

func (loader *gitRepoChartLoader) cloneRepo(
	repo *sourcev1.GitRepository,
	repoURL string,
) (string, error) {
	repoPath, err := getCachePathForRepo(loader.cacheRoot, repoURL)
	if err != nil {
		return "", fmt.Errorf(
			"unable to get cache path for Git repository %s: %w",
			repoURL,
			err,
		)
	}

	parsedURL, err := url.Parse(repoURL)
	if err != nil {
		return "", fmt.Errorf(
			"unable to parse URL %s for GitRepository %s/%s: %w",
			repoURL,
			repo.Namespace,
			repo.Name,
			err,
		)
	}

	repoCreds, err := loader.credentials.FindForRepo(parsedURL)
	if err != nil {
		return "", fmt.Errorf(
			"unable to find credentials for repository %s: %w",
			repoURL,
			err,
		)
	}

	var authOpts *git.AuthOptions
	var credentials map[string][]byte

	if repoCreds != nil {
		if parsedURL.Scheme == "ssh" &&
			repoCreds.Credentials["password"] != "" &&
			repoCreds.Credentials["identity"] == "" {
			// Re-write the URL to an HTTPS one.
			parsedURL.Scheme = "https"
			parsedURL.Host = parsedURL.Hostname()
			parsedURL.User = nil
			repoURL = parsedURL.String()
		}
		credentials = repoCreds.AsBytesMap()
	} else {
		credentials = nil
	}

	authOpts, err = git.NewAuthOptions(*parsedURL, credentials)
	if err != nil {
		return "", fmt.Errorf(
			"unable to initialize Git auth options for Git repository %s/%s: %w",
			repo.Namespace,
			repo.Name,
			err,
		)
	}

	clientOpts := []gogit.ClientOption{
		gogit.WithDiskStorage(),
		gogit.WithSingleBranch(true),
	}

	timeout := 60 * time.Second
	specTimeout := repo.Spec.Timeout
	if specTimeout != nil {
		timeout = specTimeout.Duration
	}

	client, err := loader.gitClientFactory(repoPath, authOpts, clientOpts...)
	if err != nil {
		return "", fmt.Errorf(
			"unable to create Git client to clone repository %s: %w",
			repoURL,
			err,
		)
	}
	cloneCtx, cancel := context.WithTimeout(loader.ctx, timeout)
	defer cancel()

	cloneOpts := repository.CloneConfig{
		ShallowClone: true,
	}
	if repo.Spec.Reference != nil {
		ref := normalizeGitReference(repo.Spec.Reference)
		cloneOpts.CheckoutStrategy = repository.CheckoutStrategy{
			Branch:  ref.Branch,
			Tag:     ref.Tag,
			SemVer:  ref.SemVer,
			RefName: ref.Name,
			Commit:  ref.Commit,
		}
	}
	_, err = client.Clone(cloneCtx, repoURL, cloneOpts)
	if err != nil {
		return "", fmt.Errorf(
			"unable to clone Git repository %s: %w",
			repoURL,
			err,
		)
	}
	return repoPath, nil
}

func (loader *gitRepoChartLoader) loadRepositoryChart(
	repoNode *yaml.RNode,
	repoURL string,
	parentContext *chartContext,
	chartName string,
	chartVersionSpec string,
) (*chart.Chart, error) {
	loader.logger.
		With(
			"repoName", repoNode.GetName(),
			"repoNamespace", repoNode.GetNamespace(),
			"name", chartName,
		).
		Debug("Loading chart from Git repository")

	var repo sourcev1.GitRepository

	err := decodeToObject(repoNode, &repo)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to decode GitRepository %s/%s: %w",
			repoNode.GetNamespace(),
			repoNode.GetName(),
			err,
		)
	}

	if repoURL == "" {
		repoURL = repo.Spec.URL
	}
	ref := normalizeGitReference(repo.Spec.Reference)
	chartKey := fmt.Sprintf(
		"%s#%s#%s#%s#%s#%s#%s",
		repoURL,
		chartName,
		ref.Branch,
		ref.Tag,
		ref.SemVer,
		ref.Name,
		ref.Commit,
	)
	if loader.chartCache != nil {
		if chart, ok := loader.chartCache[chartKey]; ok {
			loader.logger.
				With(
					"repoURL", repoURL,
					"name", chartName,
					"branch", ref.Branch,
					"tag", ref.Tag,
					"semver", ref.SemVer,
					"name", ref.Name,
					"commit", ref.Commit,
				).
				Debug("Using chart from in-memory cache")
			return chart, nil
		}
	}

	var repoPath string
	if parentContext != nil {
		repoPath = parentContext.localRepoPath
	} else {
		var err error
		repoPath, err = loader.cloneRepo(&repo, repoURL)
		if err != nil {
			return nil, err
		}
	}

	chartPath := path.Join(repoPath, chartName)
	chart, err := helmloader.LoadDir(chartPath)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to load chart %s from GitRepository %s/%s: %w",
			chartName,
			repo.Namespace,
			repo.Name,
			err,
		)
	}

	// TODO(vlad): Handle relative dependency paths here.
	err = loadChartDependencies(
		loader.loaderConfig,
		chart,
		&chartContext{
			localRepoPath: repoPath,
			chartName:     chartName,
			loader:        loader,
			repoNode:      repoNode,
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to load chart dependencies for %s/%s in %s: %w",
			chartName,
			chart.Metadata.Version,
			repoURL,
			err,
		)
	}

	if loader.chartCache != nil {
		loader.chartCache[chartKey] = chart
	}

	loader.logger.
		With(
			"repoName", repoNode.GetName(),
			"repoNamespace", repoNode.GetNamespace(),
			"name", chartName,
			"version", chart.Metadata.Version,
		).
		Debug("Finished loading chart")

	return chart, nil
}

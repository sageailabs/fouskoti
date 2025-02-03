// Copyright © The Sage Group plc or its licensors.

package repository

import (
	"context"
	"fmt"
	"net/url"
	"os"
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
		(original.Branch != "" ||
			original.Tag != "" ||
			original.SemVer != "" ||
			original.Name != "" ||
			original.Commit != "") {
		return original
	}
	return &sourcev1.GitRepositoryRef{Branch: "master"}
}

func (loader *gitRepoChartLoader) cloneRepo(
	repo *sourcev1.GitRepository,
	repoURL string,
) (string, error) {
	normalizedGitRef := normalizeGitReference(repo.Spec.Reference)
	gitRefString := fmt.Sprintf(
		"%s#%s#%s#%s#%s",
		normalizedGitRef.Branch,
		normalizedGitRef.Tag,
		normalizedGitRef.SemVer,
		normalizedGitRef.Name,
		normalizedGitRef.Commit,
	)
	// Git repositories checked out at different revisions should be cached at
	// different paths in order to avoid cross revision contamination.
	repoPath := path.Join(getCachePathForRepo(loader.cacheRoot, repoURL), gitRefString)

	if stat, err := os.Stat(repoPath); err == nil && stat.IsDir() {
		loader.logger.Debug("Using cached Git repository")
		return repoPath, nil
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
		CheckoutStrategy: repository.CheckoutStrategy{
			Branch:  normalizedGitRef.Branch,
			Tag:     normalizedGitRef.Tag,
			SemVer:  normalizedGitRef.SemVer,
			RefName: normalizedGitRef.Name,
			Commit:  normalizedGitRef.Commit,
		},
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
	savedLogger := loader.logger
	defer func() { loader.logger = savedLogger }()

	loader.logger = loader.logger.With(
		"namespace", repoNode.GetNamespace(),
		"name", repoNode.GetName(),
		"chart", chartName,
	)
	loader.logger.Debug("Loading chart from Git repository")

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
		loader.logger = loader.logger.With("url", repoURL)
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
					"url", repoURL,
					"branch", ref.Branch,
					"tag", ref.Tag,
					"semver", ref.SemVer,
					"ref", ref.Name,
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

	loader.logger = loader.logger.WithGroup("deps")
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
		With("version", chart.Metadata.Version).
		Debug("Finished loading chart")

	return chart, nil
}

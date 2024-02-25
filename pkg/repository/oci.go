package repository

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	sourcev1beta2 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/google/go-containerregistry/pkg/authn"
	"helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	helmgetter "helm.sh/helm/v3/pkg/getter"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/fluxcd/pkg/oci/auth/aws"
	"github.com/fluxcd/pkg/version"
	"helm.sh/helm/v3/pkg/registry"
)

var ociSchemePrefix string = fmt.Sprintf("%s://", registry.OCIScheme)

type ociRepoChartLoader struct {
	loaderConfig
}

func newOciRepositoryLoader(config loaderConfig) repositoryLoader {
	return &ociRepoChartLoader{loaderConfig: config}
}

func (loader *ociRepoChartLoader) loadRepositoryChart(
	repoNode *yaml.RNode,
	chartName string,
	chartVersion string,
) (*chart.Chart, error) {
	var repo sourcev1beta2.HelmRepository

	err := decodeToObject(repoNode, &repo)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to decode OCIRepository %s/%s: %w",
			repoNode.GetNamespace(),
			repoNode.GetName(),
			err,
		)
	}

	normalizedURL, err := normalizeURL(repo.Spec.URL)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid Helm repository URL %s: %w",
			repo.Spec.URL,
			err,
		)
	}

	return loader.loadChartByURL(
		normalizedURL,
		chartName,
		chartVersion,
	)
}

func (loader *ociRepoChartLoader) awsLogin(registryHost string) (*authn.AuthConfig, error) {
	authenticator, err := aws.NewClient().Login(loader.ctx, true, registryHost)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to log into repository %s: %w",
			registryHost,
			err,
		)
	}
	authConfig, err := authenticator.Authorization()
	if err != nil {
		return nil, fmt.Errorf(
			"unable to log into repository %s: %w",
			registryHost,
			err,
		)
	}
	return authConfig, nil
}

func getLatestMatchingVersion(
	tags []string,
	chartVersion string,
) (string, error) {
	versionString := chartVersion
	if versionString == "" {
		versionString = "*"
	}

	versionConstraint, err := semver.NewConstraint(versionString)
	if err != nil {
		return "", fmt.Errorf("unable to parse version constraint '%s'", chartVersion)
	}

	matchingVersions := make([]*semver.Version, 0, len(tags))
	for _, tag := range tags {
		version, err := version.ParseVersion(tag)
		if err != nil {
			continue
		}
		if !versionConstraint.Check(version) {
			continue
		}
		matchingVersions = append(matchingVersions, version)
	}

	if len(matchingVersions) == 0 {
		return "", fmt.Errorf(
			"unable to find version matching provided version string '%s'",
			chartVersion,
		)
	}
	sort.Sort(sort.Reverse(semver.Collection(matchingVersions)))
	return matchingVersions[0].Original(), nil
}

func (loader *ociRepoChartLoader) getChartVersion(
	client *registry.Client,
	repoURL string,
	chartName string,
	chartVersion string,
) (string, error) {
	if _, err := version.ParseVersion(chartVersion); err == nil {
		return chartVersion, nil
	}

	chartRef := path.Join(strings.TrimPrefix(repoURL, ociSchemePrefix), chartName)
	tags, err := client.Tags(chartRef)
	if err != nil {
		return "", fmt.Errorf("unable to fetch tags for %s: %w", chartRef, err)
	}
	if len(tags) == 0 {
		return "", fmt.Errorf("unable to locate any tags for %s: %w", chartRef, err)
	}

	result, err := getLatestMatchingVersion(tags, chartVersion)
	if err != nil {
		return "", fmt.Errorf(
			"unable to find latest tag for chart %s: %w",
			chartRef,
			err,
		)
	}
	return result, nil
}

func (loader *ociRepoChartLoader) loadChartByURL(
	repoURL string,
	chartName string,
	chartVersion string,
) (*chart.Chart, error) {
	loader.logger.
		With(
			"repoURL", repoURL,
			"name", chartName,
			"version", chartVersion,
		).
		Debug("Loading chart from OCI Helm repository")

	// TODO(vlad): Implement chart caching.
	_, err := getCachePathForRepo(loader.cacheRoot, repoURL)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to get cache path for Helm repository %s: %w",
			repoURL,
			err,
		)
	}

	parsedURL, err := url.Parse(repoURL)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to parse repository URL %s: %w",
			repoURL,
			err,
		)
	}

	registryClient, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf(
			"unable to create registry client: %w",
			err,
		)
	}

	authConfig, err := loader.awsLogin(parsedURL.Host)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to log in to AWS registry %s: %w",
			parsedURL.Host,
			err,
		)
	}

	err = registryClient.Login(
		parsedURL.Host,
		registry.LoginOptBasicAuth(authConfig.Username, authConfig.Password),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to log in to registry %s: %w",
			parsedURL.Host,
			err,
		)
	}

	version, err := loader.getChartVersion(
		registryClient,
		repoURL,
		chartName,
		chartVersion,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to find version %s for chart %s in repository %s: %w",
			chartVersion,
			chartName,
			repoURL,
			err,
		)
	}

	getter, err := helmgetter.NewOCIGetter(
		helmgetter.WithRegistryClient(registryClient),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to create Helm getter for %s: %w",
			repoURL,
			err,
		)
	}

	chartRef := fmt.Sprintf(
		"%s:%s",
		path.Join(strings.TrimPrefix(repoURL, ociSchemePrefix), chartName),
		version,
	)

	chartData, err := getter.Get(chartRef)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to download chart %s for version constraint %s: %w",
			chartRef,
			chartVersion,
			err,
		)
	}

	chart, err := helmloader.LoadArchive(chartData)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to load chart %s/%s in %s: %w",
			chartName,
			chartVersion,
			repoURL,
			err,
		)
	}
	err = loadChartDependencies(loader.loaderConfig, chart)
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
		With(
			"repoURL", repoURL,
			"name", chartName,
			"version", chart.Metadata.Version,
		).
		Debug("Finished loading chart")
	return chart, nil
}

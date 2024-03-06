# Helm Release Expander

Practitioners of the GitOps approach (as applied to Kubernetes) reap a number of
benefits deriving from Git being a source of truth for the state of a cluster.
One such important benefit is an ability to check the resource manifests at the
CI time for errors, security vulnerabilities, etc.  However, when using applications
deployed via Flux's `HelmRelease` resources, the CI pipeline does not see the actual
manifests that a release is expanded into and thus cannot verify them.

This project's goal is to provide a tool that expands the `HelmRelease` resources into
the resulting manifests, thus allowing the verification tools to check them.

## Running the tool

Invoke the tool with one or more arguments that are names of the files with the
input manifests. The tool will inspect the resources defined in those manifests
and, if it finds any `HelmRelease` objects, will expand them into the resulting
manifests, using charts from the repositories those `HelmRelease` objects refer
to.  If the repository resources are missing in the input, the tool will fail.
If you do not provide any input files, the tool will read from the standard
input.  The following options are available:

| Option             | Description      |
| ------------------ | ---------------- |
| --log-level        | A level threshold for logging (must be debug, info, warn, or error) |
| --log-format       | Format for the log entrie (text or json) |
| --credentials-file | A path to the file with chart repository credentials |

The `--credentials-file` option is required when there are chart repositories
that require authentication.  It must be a YAML file with a dictonariy, having
the repository URLs as keys and a dictionaries of authentication credentials as
values.  Currently, SSH Git repository URLs require two items: `identity` (a
private SSH key) and `known_hosts` (public keys for the host in the URL)

Example of a credentials file:
```yaml
ssh://git@github.com/vladlosev/fouskoti:
  identity: |
    -----BEGIN OPENSSH PRIVATE KEY-----
    <snip>
    -----END OPENSSH PRIVATE KEY-----
  known_hosts: |
    github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
    github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
    github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
```

## Plans
- Add namespace assignment for generared manifests.
- Add persistent chart caching.
- Improve authentication support for Helm and OCI repositories.
- Add recursive expansion of generated HelmRelease resources.
- Expand the README content describing the program and its usage.
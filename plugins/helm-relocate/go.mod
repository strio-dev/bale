module github.com/strio-dev/bale/plugins/helm-relocate

go 1.27.0

require (
	github.com/distribution/reference v0.6.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.1
	github.com/strio-dev/bale v0.0.0
	go.yaml.in/yaml/v3 v3.0.5
	oras.land/oras-go/v2 v2.6.3
)

require (
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

replace github.com/strio-dev/bale => ../..

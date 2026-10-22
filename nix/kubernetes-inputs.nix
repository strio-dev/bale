{ pkgs }:
let
  image =
    imageName: version: digest: hash:
    let
      # OCI transport preserves upstream manifest bytes. Docker archive transport
      # would rewrite the manifests and lose the digest used by Kubernetes.
      layout =
        pkgs.runCommand "${builtins.baseNameOf imageName}-${version}-oci"
          {
            nativeBuildInputs = [ pkgs.skopeo ];
            outputHash = hash;
            outputHashAlgo = "sha256";
            outputHashMode = "recursive";
            impureEnvVars = pkgs.lib.fetchers.proxyImpureEnvVars;
            SSL_CERT_FILE = "${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt";
          }
          ''
            skopeo --insecure-policy --override-os linux --override-arch amd64 copy \
              --preserve-digests \
              docker://${imageName}@${digest} oci:$out:${version}
          '';
    in
    {
      inherit version digest;
      reference = "${imageName}@${digest}";
      tagReference = "${imageName}:${version}";
      archive = pkgs.runCommand "${builtins.baseNameOf imageName}-${version}.oci.tar" { } ''
        tar --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
          -cf "$out" -C ${layout} .
      '';
    };
  fluxArchive = pkgs.fetchurl {
    url = "https://github.com/fluxcd/flux2/releases/download/v2.9.6/flux_2.9.6_linux_amd64.tar.gz";
    sha256 = "b4d22673e9246cbd628881f1a9ef3b090085dced291e42d804555cee8e8d42c5";
  };
in
{
  # These are Linux amd64 manifests, not multi-platform index digests.
  images = {
    source-controller =
      image "ghcr.io/fluxcd/source-controller" "v1.9.6"
        "sha256:d10ea2ebeb475de80a6d3caf792578b1eaaa0366c55442d20d7e5f6f78fad3ec"
        "sha256-YSEsM2tDB46akET306+IuAyl84ONdi/2ZoUvMf8JsqI=";
    helm-controller =
      image "ghcr.io/fluxcd/helm-controller" "v1.6.5"
        "sha256:777f34e82f901d247cab555e67cdb278a8da509cca7668f5b951bbf15b85f4ad"
        "sha256-cPiW/e5P545w9jKCLkx/rPB10dO0IKeWDEUkTgrn7ho=";
    flux-operator =
      image "ghcr.io/controlplaneio-fluxcd/flux-operator" "v0.61.0"
        "sha256:d13f764cac8b6217f5a08aa4124c8a67a3721d307afc5c41899d73a202f5bade"
        "sha256-S/uII+UTFsWjxTG48MKZlLT2UIwibA+HtLcOj8PPoKE=";
  };
  # Keep workload fixtures separate from controllers: they enter only through Bale.
  podinfo = {
    chart = ../tests/integration/testdata/helm/podinfo-6.9.4.tgz;
    images = [
      (image "ghcr.io/stefanprodan/podinfo" "6.9.4"
        "sha256:e0ca6df12c7a8931fef89253063461c0b04d149eb4bb5663130b25ef4d6d3f62"
        "sha256-zBWSAT/Tuf3XfLS7ZCj9/vprProbAOwtZKpMh33XD9U="
      )
      (image "ghcr.io/stefanprodan/podinfo" "6.9.3"
        "sha256:76fc224717fffe9e5e6e8a30ca047f64775173c0fe89b29d581ffbe28860b804"
        "sha256-CgC7g9sUtVeUtIVui1aRPfOOgW2EoXps5iE7S+Li7H8="
      )
      (image "docker.io/curlimages/curl" "7.69.0"
        "sha256:d845b6dd655da0e9ae0623065a35185f9b4438aa717fd1c9a3b89843e8b4ff26"
        "sha256-5JJEgMGihPWaNI+DQSTjDNFq2mDn1GdixfhN5+ZLhWM="
      )
      (image "docker.io/giantswarm/tiny-tools" "latest"
        "sha256:8ccea5962d6e24cf90ac726ff8af6ad25ba8dc6a691ad47cb455e3dfcd3f4c22"
        "sha256-Z8so/iXKkzyNxF/VuTn8ixIELTojDU7FBKcqcCyzdPY="
      )
      (image "docker.io/stefanprodan/grpc_health_probe" "v0.3.0"
        "sha256:0d7bd9c77ef35c3f0432142b5689ae460331a4fecfa469590fe11f3d575669e4"
        "sha256-btnKRpo5yShCg0T0UIndg+02z3ZnHFwW0tuJAhRq+Fw="
      )
    ];
  };
  fluxInstall = pkgs.runCommand "flux-2.9.6-install.yaml" { } ''
    tar -xzf ${fluxArchive} flux
    ./flux install --export --components=source-controller,helm-controller > "$out"
  '';
  operatorInstall = pkgs.fetchurl {
    url = "https://github.com/controlplaneio-fluxcd/flux-operator/releases/download/v0.61.0/install.yaml";
    hash = "sha256-VQ68juxfrgDlUzbPVAOmCEbFEXQJXfPYylQbSEgWYq8=";
  };
}

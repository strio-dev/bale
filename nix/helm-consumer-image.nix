{
  callPackage,
  helm-consumer-archive,
}:
(callPackage ./oci-artifact.nix { }) {
  name = "bale-helm-consumer-oci-${helm-consumer-archive.pluginVersion}";
  artifactType = "application/vnd.helm.plugin.v1+json";
  layers = [
    {
      file = "${helm-consumer-archive}/${helm-consumer-archive.archiveName}";
      title = helm-consumer-archive.archiveName;
      mediaType = "application/vnd.oci.image.layer.v1.tar+gzip";
    }
  ];
}

use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

// A generic artifact producer, deliberately independent of Helm and Go.
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Request {
    schema_version: u32,
    context: String,
    artifact_path: String,
    dependencies: Vec<Dependency>,
}

#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq, PartialOrd, Ord)]
#[serde(deny_unknown_fields)]
struct Dependency {
    r#ref: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct Declaration {
    schema_version: u32,
    context: String,
    dependencies: Vec<Dependency>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct Response {
    schema_version: u32,
    files: BTreeMap<String, String>,
    declaration: Declaration,
}

fn produce(input: &[u8]) -> Result<Vec<u8>, Box<dyn std::error::Error + Send + Sync>> {
    // Struct decoding rejects unknown and duplicate fields. from_slice also
    // rejects trailing JSON, invalid UTF-8, null required fields, and bad types.
    let mut request: Request = serde_json::from_slice(input)?;
    if request.schema_version != 1 {
        return Err("unsupported request schemaVersion".into());
    }
    if request.context.trim().is_empty() || request.artifact_path.is_empty() {
        return Err("nonempty context and artifactPath required".into());
    }
    for dependency in &request.dependencies {
        if dependency.r#ref.is_empty() || dependency.r#ref.chars().any(char::is_whitespace) {
            return Err("nonempty dependency reference without whitespace required".into());
        }
    }
    // OCI reference grammar is authoritatively checked by Bale's codec on the
    // host; this guest need not embed a Go-specific reference implementation.
    request.dependencies.sort();
    request.dependencies.dedup();
    let files = BTreeMap::from([(
        request.artifact_path,
        "generic dependency manifest – 構成\n".to_owned(),
    )]);
    Ok(serde_json::to_vec(&Response {
        schema_version: 1,
        files,
        declaration: Declaration {
            schema_version: 1,
            context: request.context,
            dependencies: request.dependencies,
        },
    })?)
}

fn main() {
    use std::io::{Read, Write};
    let result = (|| -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        let mut input = Vec::new();
        std::io::stdin()
            .take((8 << 20) + 1)
            .read_to_end(&mut input)?;
        if input.len() > 8 << 20 {
            return Err("input exceeds 8 MiB".into());
        }
        let output = produce(&input)?;
        if output.len() > 8 << 20 {
            return Err("output exceeds 8 MiB".into());
        }
        std::io::stdout().write_all(&output)?;
        Ok(())
    })();
    if let Err(error) = result {
        eprintln!("{error}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unicode_sort_dedupe_and_digest_are_preserved() {
        let input = include_bytes!("../../request.json");
        let output = produce(input).unwrap();
        let actual: serde_json::Value = serde_json::from_slice(&output).unwrap();
        let expected: serde_json::Value =
            serde_json::from_slice(include_bytes!("../../expected.json")).unwrap();
        assert_eq!(actual, expected);
    }

    #[test]
    fn malformed_requests_return_no_output() {
        for input in [
            br#"{}"#.as_slice(),
            br#"{"schemaVersion":1,"context":"x","artifactPath":"x","dependencies":null}"#,
            br#"{"schemaVersion":1,"context":"x","artifactPath":"x","dependencies":[],"extra":0}"#,
            br#"{"schemaVersion":1,"schemaVersion":1,"context":"x","artifactPath":"x","dependencies":[]}"#,
            br#"{"schemaVersion":1,"context":"x","artifactPath":"x","dependencies":[]} {}"#,
            br#"{"schemaVersion":2,"context":"x","artifactPath":"x","dependencies":[]}"#,
            br#"{"schemaVersion":1,"context":"x","artifactPath":"x","dependencies":[{"ref":""}]}"#,
            &[0xff, 0xfe],
        ] {
            assert!(produce(input).is_err(), "unexpected output for {input:?}");
        }
    }
}

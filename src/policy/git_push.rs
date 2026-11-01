//! git receive-pack (push) filtering.
//!
//! A push request body is a pkt-line command section — one
//! `<old-sha> <new-sha> <refname>` line per ref update, the first line
//! carrying `\0<capabilities>` — terminated by a flush pkt (`0000`), followed
//! by opaque PACK data. The proxy buffers the whole body (bounded by
//! GAP_MAX_PUSH_BODY_BYTES), parses only the command section, and either
//! forwards the original buffer untouched or answers with a synthetic
//! receive-pack result rejecting every ref.
//!
//! Implementation note: the pkt-line codec is implemented directly rather
//! than via gix-packetline. Buffering removes the incremental/streaming edge
//! cases a full library exists for; what remains is a length-prefixed frame
//! walk, exercised end-to-end against the real git binary in tests.

use std::io::Read;

use crate::config::ServeConfig;
use crate::error::ProxyError;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RefUpdate {
    pub old: String,
    pub new: String,
    pub refname: String,
}

impl RefUpdate {
    pub fn is_create(&self) -> bool {
        self.old.chars().all(|c| c == '0')
    }
    pub fn is_delete(&self) -> bool {
        self.new.chars().all(|c| c == '0')
    }
}

#[derive(Debug, Clone)]
pub struct ParsedPush {
    pub updates: Vec<RefUpdate>,
    pub capabilities: Vec<String>,
}

/// Parse the pkt-line command section at the start of a receive-pack request
/// body. Stops at the flush pkt; any PACK data after it is ignored (the
/// caller forwards the original buffer, not our parse).
pub fn parse(body: &[u8]) -> Result<ParsedPush, ProxyError> {
    let mut pos = 0usize;
    let mut updates = Vec::new();
    let mut capabilities = Vec::new();

    loop {
        if pos + 4 > body.len() {
            return Err(ProxyError::bad_request(
                "git push: truncated pkt-line length prefix",
            ));
        }
        let hex = std::str::from_utf8(&body[pos..pos + 4])
            .map_err(|_| ProxyError::bad_request("git push: non-ascii pkt-line length"))?;
        let len = u32::from_str_radix(hex, 16)
            .map_err(|_| ProxyError::bad_request("git push: invalid pkt-line length"))?
            as usize;
        pos += 4;
        if len == 0 {
            // flush pkt: end of the command section
            break;
        }
        if len < 4 {
            return Err(ProxyError::bad_request("git push: pkt-line length < 4"));
        }
        let n = len - 4;
        if pos + n > body.len() {
            return Err(ProxyError::bad_request(
                "git push: truncated pkt-line payload",
            ));
        }
        let mut line = &body[pos..pos + n];
        pos += n;

        // Capabilities ride the first command line after a NUL.
        if updates.is_empty() {
            if let Some(nul) = line.iter().position(|b| *b == 0) {
                capabilities = line[nul + 1..]
                    .split(|b| *b == b' ')
                    .filter(|c| !c.is_empty())
                    .map(|c| String::from_utf8_lossy(c).into_owned())
                    .collect();
                line = &line[..nul];
            }
        }
        let line = match line.strip_suffix(b"\n") {
            Some(l) => l,
            None => line,
        };
        let text = std::str::from_utf8(line)
            .map_err(|_| ProxyError::bad_request("git push: command line is not utf-8"))?;
        let mut parts = text.splitn(3, ' ');
        let (Some(old), Some(new), Some(refname)) = (parts.next(), parts.next(), parts.next())
        else {
            return Err(ProxyError::bad_request(format!(
                "git push: malformed command line `{text}`"
            )));
        };
        for (label, id) in [("old", old), ("new", new)] {
            let valid_len = id.len() == 40 || id.len() == 64;
            if !valid_len || !id.chars().all(|c| c.is_ascii_hexdigit()) {
                return Err(ProxyError::bad_request(format!(
                    "git push: {label} id `{id}` is not a valid object id"
                )));
            }
        }
        updates.push(RefUpdate {
            old: old.to_string(),
            new: new.to_string(),
            refname: refname.to_string(),
        });
    }

    Ok(ParsedPush {
        updates,
        capabilities,
    })
}

fn gunzip(body: &[u8]) -> Result<Vec<u8>, ProxyError> {
    let mut decoder = flate2::read::GzDecoder::new(body);
    let mut out = Vec::new();
    decoder.read_to_end(&mut out).map_err(|e| {
        ProxyError::bad_request(format!("git push: gzip body does not decode: {e}"))
    })?;
    Ok(out)
}

pub enum PushDecision {
    /// Forward the original buffered body untouched.
    Forward,
    /// Respond locally with this receive-pack result body (HTTP 200).
    Reject(Vec<u8>),
}

/// Apply the ref-prefix policy to a buffered receive-pack body.
/// `content_encoding_gzip` reflects the request's Content-Encoding header.
pub fn evaluate(
    body: &[u8],
    content_encoding_gzip: bool,
    config: &ServeConfig,
) -> Result<PushDecision, ProxyError> {
    let decoded;
    let parse_input: &[u8] = if content_encoding_gzip {
        decoded = gunzip(body)?;
        &decoded
    } else {
        body
    };
    let parsed = parse(parse_input)?;
    let ref_prefix = config.branch_ref_prefix();
    let bad: Vec<&RefUpdate> = parsed
        .updates
        .iter()
        .filter(|u| !u.refname.starts_with(&ref_prefix))
        .collect();
    if bad.is_empty() {
        return Ok(PushDecision::Forward);
    }
    let reason = format!("policy: ref must start with {ref_prefix}");
    Ok(PushDecision::Reject(rejection_body(&parsed, &reason)))
}

/// Build a well-formed receive-pack result rejecting every requested ref.
/// git renders each `ng` line as `! [remote rejected] <ref> (<reason>)` —
/// an actionable message, unlike a bare HTTP error git would swallow.
///
/// Wire format replicates git-http-backend byte-for-byte (verified against
/// git 2.39): report lines are pkt-line framed; when the client negotiated
/// a sideband, that framed report stream is itself the payload of band-1
/// frames, and the response ends with a double flush.
fn rejection_body(parsed: &ParsedPush, reason: &str) -> Vec<u8> {
    let sideband = parsed
        .capabilities
        .iter()
        .any(|c| c == "side-band-64k" || c == "side-band");
    let reason = reason.replace(['\n', '\r'], " ");

    let mut lines: Vec<String> = Vec::with_capacity(parsed.updates.len() + 1);
    lines.push("unpack ok".to_string());
    for u in &parsed.updates {
        lines.push(format!("ng {} {}", u.refname, reason));
    }

    let mut out = Vec::new();
    if sideband {
        // Matches git-http-backend byte-for-byte (verified against git 2.39
        // packet traces): one band-1 frame whose payload is the whole
        // pkt-line-framed report stream *including its terminating flush*,
        // then one raw flush to terminate the sideband stream. Without the
        // inner flush the client's report parser blocks until EOF and git
        // reports "the remote end hung up unexpectedly".
        let mut inner = vec![1u8];
        for line in lines {
            write_pkt_line(&mut inner, format!("{line}\n").as_bytes());
        }
        inner.extend_from_slice(b"0000");
        write_pkt_line(&mut out, &inner);
        out.extend_from_slice(b"0000");
    } else {
        for line in lines {
            write_pkt_line(&mut out, format!("{line}\n").as_bytes());
        }
        out.extend_from_slice(b"0000");
    }
    out
}

fn write_pkt_line(out: &mut Vec<u8>, data: &[u8]) {
    let len = data.len() + 4;
    assert!(len <= 0xffff, "pkt-line too long");
    out.extend_from_slice(format!("{len:04x}").as_bytes());
    out.extend_from_slice(data);
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::Parser;
    use flate2::write::GzEncoder;
    use flate2::Compression;
    use std::io::Write;

    const Z: &str = "0000000000000000000000000000000000000000";
    const A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const B: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

    fn config() -> ServeConfig {
        #[derive(clap::Parser)]
        struct TestCli {
            #[command(flatten)]
            serve: ServeConfig,
        }
        TestCli::parse_from(["test", "--allowed-orgs", "o", "--token", "x"])
            .serve
            .normalized()
    }

    fn pkt(data: &str) -> String {
        format!("{:04x}{data}", data.len() + 4)
    }

    fn push_body(commands: &[&str]) -> Vec<u8> {
        let mut s = String::new();
        for (i, c) in commands.iter().enumerate() {
            if i == 0 {
                s.push_str(&pkt(&format!("{c}\0report-status side-band-64k")));
            } else {
                s.push_str(&pkt(c));
            }
        }
        s.push_str("0000");
        s.push_str("PACK-not-really-but-opaque");
        s.into_bytes()
    }

    #[test]
    fn parse_single_update() {
        let body = push_body(&[&format!("{Z} {A} refs/heads/agent/x")]);
        let parsed = parse(&body).unwrap();
        assert_eq!(parsed.updates.len(), 1);
        assert!(parsed.updates[0].is_create());
        assert!(!parsed.updates[0].is_delete());
        assert_eq!(parsed.updates[0].refname, "refs/heads/agent/x");
        assert_eq!(
            parsed.capabilities,
            vec!["report-status".to_string(), "side-band-64k".to_string()]
        );
    }

    #[test]
    fn parse_delete_and_force() {
        let body = push_body(&[
            &format!("{A} {Z} refs/heads/agent/gone"),
            &format!("{A} {B} refs/heads/agent/force"),
        ]);
        let parsed = parse(&body).unwrap();
        assert!(parsed.updates[0].is_delete());
        assert!(!parsed.updates[1].is_create() && !parsed.updates[1].is_delete());
    }

    #[test]
    fn parse_garbage_errors() {
        assert!(parse(b"nope").is_err());
        assert!(parse(b"0010short").is_err());
        // not hex
        assert!(parse(b"zzzzdeadbeef").is_err());
        // valid frame, malformed command line
        let bad = pkt("not-a-command");
        assert!(parse(format!("{bad}0000").as_bytes()).is_err());
    }

    #[test]
    fn prefix_policy_allows_and_rejects() {
        let cfg = config();

        let ok = push_body(&[
            &format!("{Z} {A} refs/heads/agent/x"),
            &format!("{A} {Z} refs/heads/agent/y"), // delete within prefix ok
        ]);
        assert!(matches!(
            evaluate(&ok, false, &cfg).unwrap(),
            PushDecision::Forward
        ));

        let bad = push_body(&[
            &format!("{Z} {A} refs/heads/agent/x"),
            &format!("{Z} {A} refs/heads/main"), // one bad ref rejects all
        ]);
        match evaluate(&bad, false, &cfg).unwrap() {
            PushDecision::Reject(body) => {
                let text = String::from_utf8_lossy(&body);
                assert!(text.contains("ng refs/heads/main"), "{text}");
                assert!(text.contains("policy:"), "{text}");
            }
            PushDecision::Forward => panic!("expected rejection"),
        }
    }

    #[test]
    fn gzip_body_parses_and_policy_applies() {
        let cfg = config();
        let plain = push_body(&[&format!("{Z} {A} refs/heads/main")]);
        let mut enc = GzEncoder::new(Vec::new(), Compression::default());
        enc.write_all(&plain).unwrap();
        let gz = enc.finish().unwrap();
        match evaluate(&gz, true, &cfg).unwrap() {
            PushDecision::Reject(_) => {}
            PushDecision::Forward => panic!("expected rejection"),
        }
    }

    #[test]
    fn rejection_body_v1_plain_and_sideband() {
        let parsed = ParsedPush {
            updates: vec![RefUpdate {
                old: Z.into(),
                new: A.into(),
                refname: "refs/heads/main".into(),
            }],
            capabilities: vec!["report-status".into()],
        };
        let body = rejection_body(&parsed, "policy: nope");
        let text = String::from_utf8_lossy(&body);
        assert!(text.contains("unpack ok"));
        assert!(text.contains("ng refs/heads/main policy: nope"));
        assert!(body.ends_with(b"0000"));

        let parsed_sb = ParsedPush {
            capabilities: vec!["report-status-v2".into(), "side-band-64k".into()],
            ..parsed
        };
        let body = rejection_body(&parsed_sb, "policy: nope");
        // sideband: band-1 frames whose payload is the pkt-line-framed
        // report stream; double flush at the end (matches git-http-backend)
        let text = String::from_utf8_lossy(&body);
        assert!(text.contains("ng refs/heads/main policy: nope"), "{text}");
        assert!(body.ends_with(b"0000"));
    }
}

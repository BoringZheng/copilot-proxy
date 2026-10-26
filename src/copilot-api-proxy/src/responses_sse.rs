//! Compatibility for Copilot Responses streams whose opaque item IDs change
//! between events. Keep the first upstream ID for each output index; never
//! invent IDs or alter tool call IDs, encrypted reasoning, or output text.

use serde_json::Value;
use std::collections::HashMap;
use std::io;

const MAX_EVENT_BYTES: usize = 16 * 1024 * 1024;

#[derive(Default)]
pub(crate) struct ResponsesSse {
    pending: Vec<u8>,
    scan: usize,
    event: Vec<u8>,
    ids: HashMap<u64, String>,
    response_id: Option<String>,
}

impl ResponsesSse {
    pub(crate) fn push(&mut self, bytes: &[u8]) -> io::Result<Vec<u8>> {
        self.pending.extend_from_slice(bytes);
        let mut output = Vec::new();
        let mut consumed = 0;
        let mut i = self.scan;
        while i < self.pending.len() {
            let b = self.pending[i];
            if b != b'\r' && b != b'\n' {
                i += 1;
                continue;
            }
            // Defer a trailing CR until the next chunk to distinguish CRLF.
            if b == b'\r' && i + 1 == self.pending.len() {
                break;
            }
            let end = i + if b == b'\r' && self.pending.get(i + 1) == Some(&b'\n') {
                2
            } else {
                1
            };
            self.event.extend_from_slice(&self.pending[consumed..end]);
            if self.event.len() > MAX_EVENT_BYTES {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "Responses SSE event exceeds 16 MiB",
                ));
            }
            if i == consumed {
                output.extend_from_slice(&self.finish_event());
            }
            consumed = end;
            i = end;
        }
        if consumed > 0 {
            self.pending.drain(..consumed);
        }
        self.scan = i - consumed;
        if self.event.len() + self.pending.len() > MAX_EVENT_BYTES {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "Responses SSE event exceeds 16 MiB",
            ));
        }
        Ok(output)
    }

    pub(crate) fn finish(&mut self) -> io::Result<Vec<u8>> {
        // Resolve a deferred CR and flush a final event without a blank line.
        self.event.append(&mut self.pending);
        self.scan = 0;
        if self.event.len() > MAX_EVENT_BYTES {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "Responses SSE event exceeds 16 MiB",
            ));
        }
        Ok(self.finish_event())
    }

    fn finish_event(&mut self) -> Vec<u8> {
        let raw = std::mem::take(&mut self.event);
        let lines: Vec<&[u8]> = raw.split(|b| *b == b'\r' || *b == b'\n').collect();
        let data_lines: Vec<&[u8]> = lines
            .iter()
            .filter_map(|line| {
                line.strip_prefix(b"data:")
                    .map(|v| v.strip_prefix(b" ").unwrap_or(v))
            })
            .collect();
        let data = data_lines.join(&b'\n');
        let Ok(mut value) = serde_json::from_slice::<Value>(&data) else {
            return raw; // Comments, [DONE], and non-JSON events remain verbatim.
        };
        if !self.normalize(&mut value) {
            return raw;
        }
        let encoded = serde_json::to_vec(&value).expect("JSON Value serialization cannot fail");
        let mut output = Vec::new();
        let mut wrote_data = false;
        for line in lines {
            if line.starts_with(b"data:") {
                if !wrote_data {
                    output.extend_from_slice(b"data: ");
                    output.extend_from_slice(&encoded);
                    output.push(b'\n');
                    wrote_data = true;
                }
            } else if !line.is_empty() {
                output.extend_from_slice(line);
                output.push(b'\n');
            }
        }
        output.push(b'\n');
        output
    }

    fn stable_id(&mut self, index: u64, id: &mut Value) -> bool {
        let Some(original) = id.as_str() else {
            return false;
        };
        let first = self.ids.entry(index).or_insert_with(|| original.to_owned());
        if original == first {
            return false;
        }
        *id = Value::String(first.clone());
        true
    }

    fn normalize(&mut self, value: &mut Value) -> bool {
        let Some(kind) = value.get("type").and_then(Value::as_str) else {
            return false;
        };
        if !kind.starts_with("response.") {
            return false;
        }
        if kind == "response.created" {
            self.ids.clear();
            self.response_id = None;
        }
        let mut changed = false;
        for pointer in ["/response/id", "/response_id"] {
            if let Some(id) = value.pointer_mut(pointer) {
                if let Some(original) = id.as_str() {
                    let first = self.response_id.get_or_insert_with(|| original.to_owned());
                    if original != first {
                        *id = Value::String(first.clone());
                        changed = true;
                    }
                }
            }
        }
        if let Some(index) = value.get("output_index").and_then(Value::as_u64) {
            if let Some(id) = value.pointer_mut("/item/id") {
                changed |= self.stable_id(index, id);
            }
            if let Some(id) = value.get_mut("item_id") {
                changed |= self.stable_id(index, id);
            }
        }
        // completed, incomplete, and failed may all include final output items.
        if let Some(items) = value
            .pointer_mut("/response/output")
            .and_then(Value::as_array_mut)
        {
            for (index, item) in items.iter_mut().enumerate() {
                if let Some(id) = item.get_mut("id") {
                    changed |= self.stable_id(index as u64, id);
                }
            }
        }
        changed
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn event(v: Value) -> Vec<u8> {
        format!("data: {v}\n\n").into_bytes()
    }
    fn values(bytes: &[u8]) -> Vec<Value> {
        String::from_utf8_lossy(bytes)
            .lines()
            .filter_map(|line| {
                line.strip_prefix("data: ")
                    .and_then(|s| serde_json::from_str(s).ok())
            })
            .collect()
    }

    #[test]
    fn changing_ids_are_stable_across_every_byte_boundary_and_utf8() {
        let input = [
            event(json!({"type":"response.created","response":{"id":"resp","output":[]}})),
            event(json!({"type":"response.output_item.added","output_index":0,"item":{"id":"first","type":"message"}})),
            event(json!({"type":"response.output_text.delta","output_index":0,"item_id":"random-a","delta":"你好🌏"})),
            event(json!({"type":"response.output_text.done","output_index":0,"item_id":"random-b","text":"你好🌏"})),
            event(json!({"type":"response.output_item.done","output_index":0,"item":{"id":"random-c","type":"message"}})),
            event(json!({"type":"response.completed","response":{"id":"resp","output":[{"id":"random-d","type":"message"}]}})),
        ].concat();
        for split in 0..=input.len() {
            let mut codec = ResponsesSse::default();
            let mut out = codec.push(&input[..split]).unwrap();
            out.extend(codec.push(&input[split..]).unwrap());
            out.extend(codec.finish().unwrap());
            let v = values(&out);
            assert_eq!(v.len(), 6);
            assert_eq!(v[2]["item_id"], "first");
            assert_eq!(v[2]["delta"], "你好🌏");
            assert_eq!(v[3]["item_id"], "first");
            assert_eq!(v[4]["item"]["id"], "first");
            assert_eq!(v[5]["response"]["output"][0]["id"], "first");
            assert_eq!(v[5]["response"]["id"], "resp");
        }
        let mut codec = ResponsesSse::default();
        for b in input {
            codec.push(&[b]).unwrap();
        }
        codec.finish().unwrap();
    }

    #[test]
    fn separates_reasoning_and_tool_items_and_preserves_sensitive_payload_fields() {
        let mut codec = ResponsesSse::default();
        let mut start = json!({"type":"response.output_item.added","output_index":0,"item":{"id":"reason-first","type":"reasoning"}});
        codec.normalize(&mut start);
        let mut tool = json!({"type":"response.output_item.added","output_index":1,"item":{"id":"tool-first","type":"function_call","call_id":"call-original","arguments":"{}"}});
        codec.normalize(&mut tool);
        let mut done = json!({"type":"response.completed","response":{"output":[{"id":"reason-last","type":"reasoning","encrypted_content":"opaque-original"},{"id":"tool-last","type":"function_call","call_id":"call-original","arguments":"{}"}]}});
        codec.normalize(&mut done);
        assert_eq!(done["response"]["output"][0]["id"], "reason-first");
        assert_eq!(
            done["response"]["output"][0]["encrypted_content"],
            "opaque-original"
        );
        assert_eq!(done["response"]["output"][1]["id"], "tool-first");
        assert_eq!(done["response"]["output"][1]["call_id"], "call-original");
        assert_eq!(done["response"]["output"][1]["arguments"], "{}");
    }

    #[test]
    fn handles_crlf_cr_multiline_data_metadata_and_no_final_blank_line() {
        for ending in ["\n", "\r\n", "\r"] {
            let mut codec = ResponsesSse::default();
            codec.push(&event(json!({"type":"response.output_item.added","output_index":0,"item":{"id":"first"}}))).unwrap();
            let input = [
                ": keepalive",
                "event: response.output_text.delta",
                "id: sse-sequence",
                "retry: 1000",
                "data: {\"type\":\"response.output_text.delta\",",
                "data: \"output_index\":0,\"item_id\":\"last\",\"delta\":\"x\"}",
                "",
                "",
            ]
            .join(ending);
            let mut out = Vec::new();
            for byte in input.bytes() {
                out.extend(codec.push(&[byte]).unwrap());
            }
            out.extend(codec.finish().unwrap());
            let v = values(&out);
            assert_eq!(v.len(), 1);
            assert_eq!(v[0]["item_id"], "first");
            let text = String::from_utf8(out).unwrap();
            assert!(text.contains("id: sse-sequence\n"));
            assert!(text.contains(": keepalive\n"));
        }
        let mut codec = ResponsesSse::default();
        codec
            .push(&event(
                json!({"type":"response.output_item.added","output_index":0,"item":{"id":"first"}}),
            ))
            .unwrap();
        codec.push(b"data: {\"type\":\"response.output_text.done\",\"output_index\":0,\"item_id\":\"last\"}").unwrap();
        assert_eq!(values(&codec.finish().unwrap())[0]["item_id"], "first");
    }

    #[test]
    fn leaves_valid_streams_comments_and_non_json_verbatim_and_resets_mapping() {
        let input = [b": ping\r\n\r\ndata: [DONE]\n\ndata: broken\n\n".to_vec(),
            event(json!({"type":"response.output_item.added","output_index":0,"item":{"id":"same"}})),
            event(json!({"type":"response.output_text.delta","output_index":0,"item_id":"same","delta":"text"})),
        ].concat();
        let mut codec = ResponsesSse::default();
        assert_eq!(codec.push(&input).unwrap(), input);
        let mut v = json!({"type":"response.created","response":{"output":[]}});
        codec.normalize(&mut v);
        let mut v =
            json!({"type":"response.output_item.added","output_index":0,"item":{"id":"new"}});
        assert!(!codec.normalize(&mut v));
        assert_eq!(v["item"]["id"], "new");
    }

    #[test]
    fn rejects_unbounded_events_and_keeps_output_incremental() {
        let mut codec = ResponsesSse::default();
        let first =
            event(json!({"type":"response.output_item.added","output_index":0,"item":{"id":"a"}}));
        assert!(!codec.push(&first).unwrap().is_empty());
        assert!(codec.push(&vec![b'x'; MAX_EVENT_BYTES + 1]).is_err());
    }

    #[test]
    fn stabilizes_response_ids_and_resets_between_responses() {
        let mut codec = ResponsesSse::default();
        let mut v = json!({"type":"response.created","response":{"id":"resp-first","output":[]}});
        assert!(!codec.normalize(&mut v));
        let mut v = json!({"type":"response.in_progress","response":{"id":"resp-middle"}});
        codec.normalize(&mut v);
        assert_eq!(v["response"]["id"], "resp-first");
        let mut v = json!({"type":"response.output_text.delta","response_id":"resp-other","output_index":0,"item_id":"msg","delta":"x"});
        codec.normalize(&mut v);
        assert_eq!(v["response_id"], "resp-first");
        let mut v = json!({"type":"response.incomplete","response":{"id":"resp-last","output":[{"id":"msg-other"}]}});
        codec.normalize(&mut v);
        assert_eq!(v["response"]["id"], "resp-first");
        assert_eq!(v["response"]["output"][0]["id"], "msg");
        let mut v = json!({"type":"response.created","response":{"id":"next","output":[]}});
        assert!(!codec.normalize(&mut v));
        let mut v = json!({"type":"response.completed","response":{"id":"next-final","output":[]}});
        codec.normalize(&mut v);
        assert_eq!(v["response"]["id"], "next");
    }
}

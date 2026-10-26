//! Model-native web search via Copilot Responses API with `web_search` tool.
//!
//! Uses the `/v1/responses` endpoint with `tools: [{type: "web_search"}]`.
//! GPT-5 family models support this and perform real web searches,
//! returning results with URL citations.
//!
//! Supported models (via Copilot):
//! - `gpt-5-mini` (default, cheapest)
//! - `gpt-5.1`
//! - `gpt-5.2` (more thorough, opens pages)
//! - `gpt-5.4` (most thorough)
//! - `gpt-5.4-mini`
//!
//! Page extraction falls back to Jina Reader since the Responses API
//! doesn't have an equivalent.

use super::{PageContent, SearchResult, WebBackend};
use crate::proxy::ProxyClient;
use axum::body::Bytes;
use reqwest::Client;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

pub struct ModelBackend {
    http: Client,
    proxy: Arc<ProxyClient>,
    model: String,
}

impl ModelBackend {
    pub fn new(http: Client, proxy: Arc<ProxyClient>, model: String) -> Self {
        Self { http, proxy, model }
    }

    /// Send a Responses API request with `web_search` tool.
    async fn search_via_model(&self, query: &str) -> Result<Vec<SearchResult>, String> {
        let body = serde_json::json!({
            "model": self.model,
            "input": format!(
                "Search the web for: {}. Return a list of the most relevant results.",
                query
            ),
            "tools": [{ "type": "web_search" }],
        });

        let body_bytes = Bytes::from(serde_json::to_vec(&body).map_err(|e| e.to_string())?);

        let resp = self
            .proxy
            .forward(
                "/responses",
                reqwest::Method::POST,
                body_bytes,
                Some("application/json"),
                Some("agent"),
                false,
            )
            .await
            .map_err(|e| format!("Copilot request failed: {}", e))?;

        if !resp.status().is_success() {
            let status = resp.status();
            let text = resp.text().await.unwrap_or_default();
            return Err(format!(
                "Copilot returned {}: {}",
                status,
                &text[..text.floor_char_boundary(200)]
            ));
        }

        let body: serde_json::Value = resp.json().await.map_err(|e| e.to_string())?;

        // Extract results from the response output items.
        // The response contains:
        //   output: [
        //     {type: "web_search_call", action: {type: "search", queries: [...]}},
        //     {type: "message", content: [{type: "output_text", text: "...", annotations: [...]}]}
        //   ]
        // Annotations contain the actual URL citations with titles.
        let output = body.get("output").and_then(|o| o.as_array());
        let Some(output) = output else {
            return Ok(vec![]);
        };

        let mut results = Vec::new();

        for item in output {
            let item_type = item.get("type").and_then(|t| t.as_str()).unwrap_or("");

            if item_type == "message" {
                let content = item.get("content").and_then(|c| c.as_array());
                let Some(content) = content else { continue };

                for block in content {
                    if block.get("type").and_then(|t| t.as_str()) != Some("output_text") {
                        continue;
                    }

                    let text = block.get("text").and_then(|t| t.as_str()).unwrap_or("");
                    let annotations = block.get("annotations").and_then(|a| a.as_array());

                    if let Some(annotations) = annotations {
                        for ann in annotations {
                            if ann.get("type").and_then(|t| t.as_str()) != Some("url_citation") {
                                continue;
                            }
                            let url = ann
                                .get("url")
                                .and_then(|u| u.as_str())
                                .unwrap_or("")
                                .to_string();
                            let title = ann
                                .get("title")
                                .and_then(|t| t.as_str())
                                .unwrap_or("")
                                .to_string();

                            // Extract a snippet around the citation from the text
                            let snippet = extract_citation_context(text, ann);

                            // Deduplicate by URL
                            if !url.is_empty()
                                && !results.iter().any(|r: &SearchResult| r.url == url)
                            {
                                results.push(SearchResult {
                                    title: if title.is_empty() { url.clone() } else { title },
                                    url,
                                    content: snippet,
                                });
                            }
                        }
                    }

                    // If no annotations, use the full text as a single result
                    if results.is_empty() && !text.is_empty() {
                        results.push(SearchResult {
                            title: query.to_string(),
                            url: String::new(),
                            content: text.to_string(),
                        });
                    }
                }
            }
        }

        Ok(results)
    }
}

/// Extract text around a citation's position in the message.
fn extract_citation_context(text: &str, annotation: &serde_json::Value) -> String {
    let start = annotation
        .get("start_index")
        .and_then(|s| s.as_u64())
        .unwrap_or(0) as usize;
    let end = annotation
        .get("end_index")
        .and_then(|e| e.as_u64())
        .unwrap_or(0) as usize;

    // Get text before the citation marker as context
    if start > 0 && start <= text.len() {
        // Walk back to find the sentence/paragraph containing this citation
        let before = &text[..text.floor_char_boundary(start)];
        // Find the last paragraph or sentence break
        let context_start = before
            .rfind("\n\n")
            .map(|p| p + 2)
            .or_else(|| before.rfind(". ").map(|p| p + 2))
            .unwrap_or(before.len().saturating_sub(200));
        let snippet = &before[text.floor_char_boundary(context_start)..];
        // Clean up markdown citation markers like ([source](url))
        let cleaned = snippet
            .trim()
            .trim_end_matches(['(', '['])
            .trim()
            .to_string();
        if !cleaned.is_empty() {
            return cleaned;
        }
    }

    // Fallback: use text around the end index
    if end > 0 && end < text.len() {
        let safe_end = text.ceil_char_boundary(end.min(text.len()));
        let safe_start = text.floor_char_boundary(safe_end.saturating_sub(200));
        return text[safe_start..safe_end].trim().to_string();
    }

    String::new()
}

impl WebBackend for ModelBackend {
    fn search(
        &self,
        queries: Vec<String>,
        max_results: usize,
    ) -> Pin<Box<dyn Future<Output = Result<Vec<SearchResult>, String>> + Send + '_>> {
        Box::pin(async move {
            let mut results = Vec::new();

            for query in &queries {
                if query.is_empty() {
                    continue;
                }
                match self.search_via_model(query).await {
                    Ok(r) => results.extend(r),
                    Err(e) => tracing::warn!("Model search failed for {:?}: {}", query, e),
                }
                if results.len() >= max_results {
                    break;
                }
            }

            results.truncate(max_results);
            Ok(results)
        })
    }

    fn extract_page(
        &self,
        url: String,
    ) -> Pin<Box<dyn Future<Output = Result<PageContent, String>> + Send + '_>> {
        // Responses API doesn't have page extraction — fall back to Jina.
        Box::pin(async move {
            let jina = super::jina::JinaBackend::new(self.http.clone());
            jina.do_extract_page(&url).await
        })
    }
}

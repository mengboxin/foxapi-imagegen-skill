---
name: foxapi-imagegen
description: "Generate or edit images through FoxAPI's OpenAI-compatible image APIs, including Responses image_generation and the Images generations/edits endpoints."
---

# FoxAPI Imagegen

Use the prebuilt Go client in `bin/<os>-<arch>/foxapi-imagegen` (`.exe` on Windows). Binaries are included for Windows, macOS, and Linux on amd64 and arm64, so those platforms do not need Python or Go installed. On other architectures, `scripts/foxapi_imagegen.py` is a Responses-only fallback; Images API routing requires a prebuilt Go client.

## Choose an API and model

- Default to FoxAPI's Responses API (`POST /v1/responses`) with the `image_generation` tool.
- Use `--api-mode images` when the selected model is documented for the OpenAI Images API (`/v1/images/generations` or `/v1/images/edits`). The default Images endpoint is derived from `FOXAPI_BASE_URL` or `https://foxapi.cn/v1`; override it with `--images-endpoint` or `FOXAPI_IMAGES_URL`.
- Preserve exact model IDs, including `gpt-image-2.5-sunburst` and `gpt-image-2.5-flare`. Do not infer an endpoint from a display name: follow FoxAPI's model documentation or the user's requested API mode.
- The default `--model auto` discovers a model on every run. Responses mode selects the newest catalog GPT model by creation time; Images mode selects the newest catalog ID beginning `gpt-image-`, `dall-e-`, or `dalle-`. The catalog does not expose endpoint/tool compatibility, so pin an exact model ID if automatic selection is unsuitable. Other image-model naming schemes work by passing their exact ID.

## Generate or edit

Example on Windows x64:

~~~powershell
bin/windows-amd64/foxapi-imagegen.exe generate --prompt "A clean product illustration" --cwd "<current workspace>"
~~~

Use the matching `bin/<os>-<arch>` binary on other supported systems. For an Images API model, add `--api-mode images`; for example:

~~~powershell
bin/windows-amd64/foxapi-imagegen.exe generate --api-mode images --model gpt-image-2.5-sunburst --prompt "A clean product illustration" --request-params-json '{"output_compression":85}'
~~~

Only pass extra fields and values accepted by the selected model. `--tool-params-json` / `--tool-param` add fields inside the Responses `image_generation` tool. `--request-params-json` / `--request-param` add top-level fields to an Images API request. The two parameter groups cannot be mixed. JSON values keep their types; for example, numbers remain numbers and arrays remain arrays. Repeated `--n` requests generate images one at a time.

Responses mode accepts multiple local `--image` references and `--image-url` references. Images API edit mode accepts one local `--image` reference and uses `/images/edits`; for multiple references or remote image URLs, use Responses mode.

## Credentials and settings

Never print API keys. The client checks, in order:

1. `--api-key`
2. `FOXAPI_API_KEY` or `FOXAPI_KEY`
3. The local setup file `%USERPROFILE%/.codex/foxapi-imagegen/config.json` or `~/.codex/foxapi-imagegen/config.json`
4. `OPENAI_API_KEY`
5. API-key fields in Codex `auth.json`

If no credential is available, ask the user to run `npx --yes foxapi-imagegen-skill setup` once or set `FOXAPI_API_KEY`. Do not switch to another image provider.

Useful options and environment variables:

- `--api-mode responses|images` or `FOXAPI_IMAGE_API_MODE`
- `--model auto|MODEL_ID` or `FOXAPI_RESPONSES_MODEL` (Responses), `FOXAPI_IMAGE_API_MODEL` (Images), `FOXAPI_IMAGE_MODEL`, `FOXAPI_MODEL`
- `--size WIDTHxHEIGHT`, `--quality VALUE`, `--background VALUE`, `--output-format png|jpeg|webp`
- `--endpoint URL` / `FOXAPI_RESPONSES_URL`, `--images-endpoint URL` / `FOXAPI_IMAGES_URL`, or `--base-url URL` / `FOXAPI_BASE_URL`
- `--tool-params-json '{"field":"value"}'` for Responses tool fields
- `--request-params-json '{"field":"value"}'` for Images API fields
- `--output-dir PATH` or `FOXAPI_IMAGE_OUTPUT_DIR`
- `--image PATH` and `--image-url URL` for reference images; `--n 2` for two generated images

## Results

The client prints JSON. On success, include each returned `markdown` image line, `saved_markdown` line, and `folder_markdown` line in the reply. Mention `actual_size` or `actual_quality` when returned values differ from the request. Never show Base64 or raw SSE data.

By default, images are saved to `<current workspace>/outputs/foxapi-imagegen/`. Use the API response's actual dimensions when an exact output size or aspect ratio matters; requested values may be adjusted by the model.

Explain common failures in Chinese: missing/invalid credentials (401), permission issues (403), insufficient balance/quota (402), unsupported model/parameter, missing image result, or unreadable reference path. Keep the API's useful error message without exposing credentials.

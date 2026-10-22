# FoxAPI Imagegen Skill

This release installs a FoxAPI image-generation skill with prebuilt Go clients. Windows, macOS, and Linux users on amd64 or arm64 do not need Python or Go installed. The Python fallback for other architectures supports Responses mode only.

## Install

~~~powershell
npx --yes mengboxin/foxapi-imagegen-skill setup --force
~~~

The command installs the latest version from this public GitHub repository, replaces the skill files, and prompts once to save a FoxAPI API key locally. Restart Codex after installation.

## API and model selection

Responses API is the default and sends `image_generation` requests to `/v1/responses`. Use `--api-mode images` for models documented for the OpenAI Images API. The client automatically selects the newest catalog GPT model in Responses mode, or the newest `gpt-image-*`, `dall-e-*`, or `dalle-*` model in Images mode. Pass `--model` with an exact ID such as `gpt-image-2.5-sunburst` to pin a model. The FoxAPI model catalog does not expose API compatibility, so auto-selection may need to be pinned when a model is unavailable on the chosen route.

## Model-specific parameters

Responses tool fields and Images API request fields are passed separately:

~~~powershell
bin/windows-amd64/foxapi-imagegen.exe generate --model gpt-image-2.5-sunburst --prompt "A clean product illustration" --tool-params-json '{"parameter":"value"}'
bin/windows-amd64/foxapi-imagegen.exe generate --api-mode images --model gpt-image-2.5-sunburst --prompt "A clean product illustration" --request-params-json '{"output_compression":85}'
~~~

Use only parameters and values accepted by FoxAPI for the selected model. Repeated `--tool-param key=value` or `--request-param key=value` flags are also available. Images API edit mode accepts one local reference image; Responses mode supports multiple local images and image URLs.

Use `FOXAPI_RESPONSES_MODEL` to set the Responses default, or `FOXAPI_IMAGE_API_MODEL` to set the Images API default. `FOXAPI_IMAGE_MODEL` and `FOXAPI_MODEL` work as shared fallbacks.

## Run without installing

On Windows x64:

~~~powershell
%USERPROFILE%\.codex\skills\foxapi-imagegen\bin\windows-amd64\foxapi-imagegen.exe generate --prompt "A clean product illustration"
~~~

The matching prebuilt clients are included for Windows, macOS, and Linux on amd64 and arm64. Credentials are read from the setup file or supported API-key environment variables. Generated images are saved under `outputs/foxapi-imagegen` in the current workspace by default.

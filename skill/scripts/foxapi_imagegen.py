#!/usr/bin/env python3
"""Generate images through FoxAPI Responses image_generation."""

from __future__ import annotations

import argparse
import base64
import json
import mimetypes
import os
import re
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Iterable
from urllib.parse import urlsplit, urlunsplit


DEFAULT_BASE_URL = "https://foxapi.cn/v1"
DEFAULT_MODEL = "auto"
DEFAULT_SIZE = "1024x1024"
DEFAULT_QUALITY = "high"
DEFAULT_OUTPUT_FORMAT = "png"
OUTPUT_SUBDIR = Path("outputs") / "foxapi-imagegen"
USER_AGENT = "codex-foxapi-imagegen/1.0"


class ImagegenError(Exception):
    def __init__(self, error_type: str, message: str) -> None:
        super().__init__(message)
        self.error_type = error_type
        self.message = message


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        result = run(args)
    except ImagegenError as exc:
        result = {"ok": False, "error_type": exc.error_type, "message": exc.message}
    except Exception as exc:  # noqa: BLE001 - command boundary
        result = {
            "ok": False,
            "error_type": "unexpected_error",
            "message": f"Unexpected image generation error: {exc}",
        }
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result.get("ok") else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Generate images through FoxAPI Responses.")
    subparsers = parser.add_subparsers(dest="mode", required=True)

    generate = subparsers.add_parser("generate", help="Text-to-image or reference-image generation.")
    generate.add_argument("--prompt", required=True, help="Image prompt.")
    generate.add_argument(
        "--image",
        action="append",
        default=[],
        help="Reference image file path. Can be repeated.",
    )
    generate.add_argument(
        "--image-url",
        action="append",
        default=[],
        help="Reference image URL or data URL. Can be repeated.",
    )
    generate.add_argument("--cwd", help="Current Codex workspace folder.")
    generate.add_argument("--output-dir", help="Explicit output folder.")
    generate.add_argument(
        "--model",
        default=first_env(["FOXAPI_RESPONSES_MODEL", "FOXAPI_IMAGE_MODEL", "FOXAPI_MODEL"], DEFAULT_MODEL),
        help="Responses model ID, or 'auto' to use the newest listed GPT model.",
    )
    generate.add_argument(
        "--size",
        default=first_env(["FOXAPI_IMAGE_SIZE"], DEFAULT_SIZE),
        help="Requested size, e.g. 1024x1024.",
    )
    generate.add_argument(
        "--quality",
        default=first_env(["FOXAPI_IMAGE_QUALITY"], DEFAULT_QUALITY),
        help="Requested quality.",
    )
    generate.add_argument("--background", help="Optional image background setting.")
    generate.add_argument("--tool-params-json", default="", help="Extra image_generation tool parameters as a JSON object.")
    generate.add_argument("--tool-param", action="append", default=[], help="Extra tool parameter as key=value; repeatable.")
    generate.add_argument("--api-key", help="API key override. Prefer environment variables.")
    generate.add_argument("--endpoint", help="Full Responses endpoint, e.g. https://foxapi.cn/v1/responses.")
    generate.add_argument("--base-url", help="Base URL, default https://foxapi.cn/v1.")
    generate.add_argument(
        "--output-format",
        default=first_env(["FOXAPI_IMAGE_OUTPUT_FORMAT"], DEFAULT_OUTPUT_FORMAT),
    )
    generate.add_argument("--n", type=int, default=1, help="Number of images to generate.")
    generate.add_argument("--timeout", type=int, default=240)
    generate.add_argument("--mock-b64", help="Offline test hook: save this Base64 image.")
    return parser


def run(args: argparse.Namespace) -> dict[str, Any]:
    validate_args(args)
    output_dir = resolve_output_dir(args)
    output_dir.mkdir(parents=True, exist_ok=True)

    if args.mock_b64:
        api_key = ""
        credential_source = "mock_b64"
    else:
        api_key, credential_source = resolve_api_key(args)
        args.model = resolve_model(args, api_key)

    saved: list[dict[str, Any]] = []
    for index in range(args.n):
        if args.mock_b64:
            item = {
                "result": args.mock_b64,
                "output_format": normalize_output_format(args.output_format),
                "status": "completed",
                "action": "generate",
            }
        else:
            item = request_image(args, api_key)
        saved.append(save_image_item(item, args, output_dir, index))

    paths = [entry["path"] for entry in saved]
    markdown = [to_markdown_image(path) for path in paths]
    saved_markdown = [f"Saved to: {to_markdown_link(path, path)}" for path in paths]
    folder = to_display_path(output_dir)

    return {
        "ok": True,
        "mode": "generate",
        "provider": "foxapi",
        "endpoint": resolve_responses_url(args),
        "model": args.model,
        "credential_source": credential_source,
        "requested_size": args.size,
        "requested_quality": args.quality,
        "reference_image_count": reference_image_count(args),
        "images": saved,
        "paths": paths,
        "markdown": markdown,
        "saved_markdown": saved_markdown,
        "folder_markdown": to_markdown_link("Open folder", folder),
    }


def validate_args(args: argparse.Namespace) -> None:
    if args.n < 1 or args.n > 4:
        raise ImagegenError("invalid_n", "--n must be between 1 and 4.")
    if not re.fullmatch(r"\d+x\d+", args.size):
        raise ImagegenError("invalid_size", "Use WIDTHxHEIGHT for --size, for example 1024x1024.")
    tool_params = parse_tool_params(args)
    custom_format = tool_params.get("output_format")
    if isinstance(custom_format, str):
        args.output_format = normalize_output_format(custom_format)
    if args.output_format.lower() not in {"png", "jpg", "jpeg", "webp"}:
        raise ImagegenError("invalid_output_format", "--output-format must be png, jpg, jpeg, or webp.")
    if args.timeout < 1:
        raise ImagegenError("invalid_timeout", "--timeout must be positive.")
    for image in args.image:
        path = Path(image).expanduser()
        if not path.is_file():
            raise ImagegenError("unreadable_reference_image", f"Reference image is not readable: {image}")


def resolve_output_dir(args: argparse.Namespace) -> Path:
    if args.output_dir:
        return Path(args.output_dir).expanduser()
    env_dir = os.getenv("FOXAPI_IMAGE_OUTPUT_DIR")
    if env_dir:
        return Path(env_dir).expanduser()
    cwd = Path(args.cwd).expanduser() if args.cwd else Path.cwd()
    return cwd / OUTPUT_SUBDIR


def resolve_api_key(args: argparse.Namespace) -> tuple[str, str]:
    candidates = [
        ("--api-key", args.api_key),
        ("FOXAPI_API_KEY", os.getenv("FOXAPI_API_KEY")),
        ("FOXAPI_KEY", os.getenv("FOXAPI_KEY")),
        ("foxapi_config", read_saved_api_key()),
        ("OPENAI_API_KEY", os.getenv("OPENAI_API_KEY")),
    ]
    for source, value in candidates:
        if value:
            return value, source

    auth_key = read_codex_auth_json_key()
    if auth_key:
        return auth_key, "codex_auth_json"

    raise ImagegenError(
        "missing_api_key",
        "No API key found. Run the Skill setup command, or set FOXAPI_API_KEY / OPENAI_API_KEY.",
    )


def read_saved_api_key() -> str | None:
    """Read the explicit FoxAPI credential saved by the package setup command."""
    paths: list[Path] = []
    codex_home = os.getenv("CODEX_HOME")
    if codex_home:
        paths.append(Path(codex_home).expanduser() / "foxapi-imagegen" / "config.json")
    paths.append(Path.home() / ".codex" / "foxapi-imagegen" / "config.json")

    seen: set[Path] = set()
    for path in paths:
        path = path.resolve()
        if path in seen:
            continue
        seen.add(path)
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError, UnicodeDecodeError):
            continue
        if not isinstance(data, dict):
            continue
        for name in ("apiKey", "FOXAPI_API_KEY", "FOXAPI_KEY"):
            value = data.get(name)
            if isinstance(value, str) and value.strip():
                return value.strip()
    return None


def read_codex_auth_json_key() -> str | None:
    paths: list[Path] = []
    codex_home = os.getenv("CODEX_HOME")
    if codex_home:
        paths.append(Path(codex_home).expanduser() / "auth.json")
    paths.append(Path.home() / ".codex" / "auth.json")

    for path in paths:
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError, UnicodeDecodeError):
            continue
        value = find_api_key(data)
        if value:
            return value
    return None


def find_api_key(value: Any) -> str | None:
    names = {"OPENAI_API_KEY", "FOXAPI_API_KEY", "FOXAPI_KEY", "api_key", "openai_api_key", "foxapi_api_key"}
    if isinstance(value, dict):
        for key, item in value.items():
            if key in names and isinstance(item, str) and item.strip():
                return item.strip()
        for item in value.values():
            found = find_api_key(item)
            if found:
                return found
    elif isinstance(value, list):
        for item in value:
            found = find_api_key(item)
            if found:
                return found
    return None


def normalize_base_url(raw: str | None) -> str:
    base_url = raw or os.getenv("FOXAPI_BASE_URL") or os.getenv("OPENAI_BASE_URL") or DEFAULT_BASE_URL
    base_url = base_url.rstrip("/")
    if base_url.endswith("/v1"):
        return base_url
    return f"{base_url}/v1"


def resolve_responses_url(args: argparse.Namespace) -> str:
    endpoint = args.endpoint or os.getenv("FOXAPI_RESPONSES_URL")
    if endpoint:
        endpoint = endpoint.rstrip("/")
        if endpoint.endswith("/responses"):
            return endpoint
        if endpoint.endswith("/v1"):
            return f"{endpoint}/responses"
        return f"{endpoint}/v1/responses"
    return f"{normalize_base_url(args.base_url)}/responses"


def resolve_models_url(args: argparse.Namespace) -> str:
    parts = urlsplit(resolve_responses_url(args))
    path = parts.path.rstrip("/")
    if not path.endswith("/responses"):
        raise ImagegenError("model_discovery_error", "The Responses endpoint must end with /responses to discover models.")
    models_path = f"{path[:-len('/responses')]}/models"
    return urlunsplit((parts.scheme, parts.netloc, models_path, parts.query, ""))


def resolve_model(args: argparse.Namespace, api_key: str) -> str:
    requested = args.model.strip()
    if requested.lower() not in {"auto", "latest"}:
        return requested

    url = resolve_models_url(args)
    request = urllib.request.Request(
        url=url,
        method="GET",
        headers={
            "Authorization": f"Bearer {api_key}",
            "Accept": "application/json",
            "User-Agent": USER_AGENT,
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=min(args.timeout, 20)) as response:
            catalog = json.load(response)
    except urllib.error.HTTPError as exc:
        raise ImagegenError(
            "model_discovery_error",
            f"Could not read the FoxAPI model catalog (HTTP {exc.code}). Set --model to a specific model ID to continue.",
        ) from exc
    except urllib.error.URLError as exc:
        raise ImagegenError(
            "model_discovery_error",
            f"Could not reach the FoxAPI model catalog: {exc.reason}. Set --model to a specific model ID to continue.",
        ) from exc
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise ImagegenError("model_discovery_error", "FoxAPI returned an unreadable model catalog.") from exc

    entries = catalog.get("data", catalog.get("models", [])) if isinstance(catalog, dict) else catalog
    if not isinstance(entries, list):
        raise ImagegenError("model_discovery_error", "FoxAPI returned an invalid model catalog.")

    candidates = [entry for entry in entries if isinstance(entry, dict) and is_general_gpt_model(entry.get("id"))]
    if not candidates:
        raise ImagegenError("model_discovery_error", "FoxAPI did not list a general GPT model. Set --model to a specific model ID.")

    return max(candidates, key=model_catalog_rank)["id"]


def is_general_gpt_model(model_id: Any) -> bool:
    if not isinstance(model_id, str) or not re.fullmatch(r"gpt-\d+(?:\.\d+)?(?:-[a-z0-9]+(?:-[a-z0-9]+)*)?", model_id, re.IGNORECASE):
        return False
    excluded = {"image", "audio", "tts", "transcribe", "transcription", "codex", "embedding", "realtime", "whisper"}
    return not (set(model_id.lower().split("-")) & excluded)


def model_catalog_rank(entry: dict[str, Any]) -> tuple[int, tuple[int, ...], int, str]:
    model_id = str(entry["id"])
    try:
        created = int(entry.get("created") or 0)
    except (TypeError, ValueError):
        created = 0

    version_match = re.match(r"^gpt-(\d+(?:\.\d+)?)", model_id, re.IGNORECASE)
    version = tuple(int(part) for part in version_match.group(1).split(".")) if version_match else ()
    variant_priority = {"astra": 4, "sol": 3, "terra": 2, "luna": 1}
    variant = next((variant_priority[token] for token in model_id.lower().split("-") if token in variant_priority), 0)
    return created, version, variant, model_id


def first_env(names: list[str], fallback: str) -> str:
    for name in names:
        value = os.getenv(name)
        if value:
            return value
    return fallback


def normalize_output_format(value: str) -> str:
    fmt = value.lower()
    return "jpeg" if fmt == "jpg" else fmt


def request_image(args: argparse.Namespace, api_key: str) -> dict[str, Any]:
    url = resolve_responses_url(args)
    tool: dict[str, Any] = {
        "type": "image_generation",
        "action": "generate",
        "size": args.size,
        "quality": args.quality,
    }
    if args.background:
        tool["background"] = args.background
    if args.output_format:
        tool["output_format"] = normalize_output_format(args.output_format)
    tool.update(parse_tool_params(args))

    payload = {
        "model": args.model,
        "input": build_input(args),
        "tools": [tool],
        "tool_choice": {"type": "image_generation"},
        "stream": True,
    }
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    request = urllib.request.Request(
        url=url,
        data=body,
        method="POST",
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
            "User-Agent": USER_AGENT,
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=args.timeout) as response:
            return read_image_item_from_sse(response)
    except urllib.error.HTTPError as exc:
        detail = safe_read_error(exc)
        raise ImagegenError("http_error", f"FoxAPI returned HTTP {exc.code}: {detail}") from exc
    except urllib.error.URLError as exc:
        raise ImagegenError("network_error", f"Could not reach FoxAPI: {exc.reason}") from exc


def parse_tool_params(args: argparse.Namespace) -> dict[str, Any]:
    extra: dict[str, Any] = {}
    if args.tool_params_json:
        try:
            parsed = json.loads(args.tool_params_json)
        except json.JSONDecodeError as exc:
            raise ImagegenError("invalid_tool_params", "--tool-params-json must be a valid JSON object.") from exc
        if not isinstance(parsed, dict):
            raise ImagegenError("invalid_tool_params", "--tool-params-json must be a JSON object.")
        extra.update(parsed)

    for pair in args.tool_param:
        key, separator, value = pair.partition("=")
        key = key.strip()
        if not separator or not key:
            raise ImagegenError("invalid_tool_params", "--tool-param must use key=value.")
        try:
            parsed_value = json.loads(value)
        except json.JSONDecodeError:
            parsed_value = value
        extra[key] = parsed_value

    if any(key.lower() in {"type", "action"} for key in extra):
        raise ImagegenError("invalid_tool_params", "Tool parameters cannot replace the reserved type or action field.")
    return extra


def build_input(args: argparse.Namespace) -> str | list[dict[str, Any]]:
    if not args.image and not args.image_url:
        return args.prompt

    content: list[dict[str, str]] = [{"type": "input_text", "text": args.prompt}]
    for image in args.image:
        content.append({"type": "input_image", "image_url": encode_image_file_as_data_url(image)})
    for image_url in args.image_url:
        content.append({"type": "input_image", "image_url": image_url})

    return [{"role": "user", "content": content}]


def encode_image_file_as_data_url(image: str) -> str:
    path = Path(image).expanduser()
    try:
        image_bytes = path.read_bytes()
    except OSError as exc:
        raise ImagegenError("unreadable_reference_image", f"Could not read reference image: {image}") from exc

    mime_type = mimetypes.guess_type(path.name)[0] or "image/png"
    b64 = base64.b64encode(image_bytes).decode("ascii")
    return f"data:{mime_type};base64,{b64}"


def reference_image_count(args: argparse.Namespace) -> int:
    return len(args.image) + len(args.image_url)


def read_image_item_from_sse(lines: Iterable[bytes]) -> dict[str, Any]:
    event_name: str | None = None
    data_lines: list[str] = []
    last_error: Any = None
    last_item: dict[str, Any] | None = None

    def process_event(event: str | None, data: str) -> None:
        nonlocal last_error, last_item
        if not data or data == "[DONE]":
            return
        try:
            obj = json.loads(data)
        except json.JSONDecodeError:
            return
        error = obj.get("error")
        if error:
            last_error = error
        for item in find_image_items(obj):
            last_item = item

    for raw in lines:
        line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
        if line == "":
            process_event(event_name, "\n".join(data_lines).strip())
            event_name = None
            data_lines = []
            continue
        if line.startswith("event:"):
            event_name = line[len("event:") :].strip()
        elif line.startswith("data:"):
            data_lines.append(line[len("data:") :].lstrip())

    if data_lines:
        process_event(event_name, "\n".join(data_lines).strip())

    if last_item and last_item.get("result"):
        return last_item
    if last_error:
        raise ImagegenError("api_error", format_api_error(last_error))
    raise ImagegenError("missing_image_result", "FoxAPI response did not contain image_generation_call.result.")


def find_image_items(value: Any) -> list[dict[str, Any]]:
    found: list[dict[str, Any]] = []
    if isinstance(value, dict):
        if value.get("type") == "image_generation_call" and value.get("result"):
            found.append(value)
        for item in value.values():
            found.extend(find_image_items(item))
    elif isinstance(value, list):
        for item in value:
            found.extend(find_image_items(item))
    return found


def save_image_item(item: dict[str, Any], args: argparse.Namespace, output_dir: Path, index: int) -> dict[str, Any]:
    b64 = item.get("result")
    if not isinstance(b64, str) or not b64:
        raise ImagegenError("missing_image_result", "Image item has no Base64 result.")
    try:
        image_bytes = base64.b64decode(b64)
    except Exception as exc:  # noqa: BLE001
        raise ImagegenError("decode_error", f"Could not decode image Base64: {exc}") from exc

    output_format = normalize_output_format(str(item.get("output_format") or args.output_format))
    suffix = "jpg" if output_format == "jpeg" else output_format
    timestamp = f"{time.strftime('%Y%m%d-%H%M%S')}-{str(time.time_ns())[-6:]}"
    slug = slugify(args.prompt)
    path = output_dir / f"{timestamp}-{index + 1}-{slug}.{suffix}"
    path.write_bytes(image_bytes)

    return {
        "path": to_display_path(path),
        "bytes": len(image_bytes),
        "item_id": item.get("id"),
        "status": item.get("status"),
        "action": item.get("action"),
        "actual_size": item.get("size"),
        "actual_quality": item.get("quality"),
        "output_format": output_format,
    }


def slugify(value: str) -> str:
    value = value.strip().lower()
    value = re.sub(r"[^a-z0-9]+", "-", value)
    value = re.sub(r"-{2,}", "-", value).strip("-")
    return value[:48] or "image"


def to_display_path(path: Path | str) -> str:
    return str(Path(path).resolve()).replace("\\", "/")


def to_markdown_image(path: str) -> str:
    return f"![Generated image]({path})"


def to_markdown_link(label: str, path: str) -> str:
    return f"[{label}]({path})"


def safe_read_error(exc: urllib.error.HTTPError) -> str:
    try:
        body = exc.read().decode("utf-8", errors="replace")
    except Exception:  # noqa: BLE001
        return exc.reason
    return body[:2000]


def format_api_error(error: Any) -> str:
    if isinstance(error, dict):
        message = error.get("message") or error.get("type") or json.dumps(error, ensure_ascii=False)
        return str(message)
    return str(error)


if __name__ == "__main__":
    raise SystemExit(main())

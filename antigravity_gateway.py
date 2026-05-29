import os
import sys
import json
import argparse
import asyncio
from typing import Dict, Any, List, Optional
import urllib.request
import urllib.error

# Verify dependencies are available or installable
try:
    from fastapi import FastAPI, Request, HTTPException
    from fastapi.responses import StreamingResponse
    import uvicorn
    from pydantic import BaseModel, create_model, Field
except ImportError:
    print("Required libraries (fastapi, uvicorn, pydantic) are missing.", file=sys.stderr)
    print("Please install them using: pip install fastapi uvicorn pydantic", file=sys.stderr)
    sys.exit(1)

# Check if google-antigravity SDK is available
HAS_SDK = False
try:
    from google.antigravity import Agent, LocalAgentConfig
    HAS_SDK = True
except ImportError:
    print("google-antigravity SDK is not installed. Falling back to direct Gemini API routing.", file=sys.stderr)

app = FastAPI(title="Antigravity SDK Gateway")

def uppercase_schema_types(schema: Any) -> Any:
    """Converts JSON schema types to uppercase for Gemini API compatibility."""
    if isinstance(schema, dict):
        out = {}
        for k, v in schema.items():
            if k == "type" and isinstance(v, str):
                out[k] = v.upper()
            else:
                out[k] = uppercase_schema_types(v)
        return out
    elif isinstance(schema, list):
        return [uppercase_schema_types(item) for item in schema]
    return schema

def map_messages_to_gemini(messages: List[Dict[str, Any]]) -> tuple[Optional[str], List[Dict[str, Any]]]:
    """Maps OpenAI messages list to Gemini API system instruction and contents."""
    system_instruction = None
    contents = []

    for msg in messages:
        role = msg.get("role")
        content = msg.get("content") or ""
        
        if role == "system":
            system_instruction = content
            continue
            
        parts = []
        if isinstance(content, str) and content.strip():
            parts.append({"text": content})
        elif isinstance(content, list):
            for part in content:
                if not isinstance(part, dict):
                    continue
                part_type = part.get("type")
                if part_type == "text":
                    parts.append({"text": part.get("text", "")})
                elif part_type in ("image_url", "input_image"):
                    url = ""
                    if part_type == "image_url":
                        img_url_obj = part.get("image_url")
                        if isinstance(img_url_obj, dict):
                            url = img_url_obj.get("url", "")
                        elif isinstance(img_url_obj, str):
                            url = img_url_obj
                    else:
                        url = part.get("image_url", "")
                    
                    if url.startswith("data:image/"):
                        try:
                            header, base64_data = url.split(",", 1)
                            mime_type = header.split(";")[0].split(":", 1)[1]
                            parts.append({
                                "inlineData": {
                                    "mimeType": mime_type,
                                    "data": base64_data
                                }
                            })
                        except Exception as e:
                            print(f"Error parsing inline image: {e}", sys.stderr)
                elif part_type == "image":
                    source = part.get("source", {})
                    if source.get("type") == "base64" and "media_type" in source and "data" in source:
                        parts.append({
                            "inlineData": {
                                "mimeType": source["media_type"],
                                "data": source["data"]
                            }
                        })

        # Map tool calls (assistant message containing tool calls)
        tool_calls = msg.get("tool_calls")
        if tool_calls:
            for tc in tool_calls:
                func = tc.get("function", {})
                args = {}
                try:
                    if isinstance(func.get("arguments"), str):
                        args = json.loads(func["arguments"])
                    else:
                        args = func.get("arguments") or {}
                except Exception:
                    pass
                parts.append({
                    "functionCall": {
                        "name": func.get("name"),
                        "args": args
                    }
                })

        # Map tool response
        if role == "tool":
            role = "user"  # Gemini 2.0 maps function responses as user role parts
            tool_name = msg.get("name")
            # Wrap response in a dictionary
            resp_data = {"result": content}
            try:
                if isinstance(content, str):
                    resp_data = json.loads(content)
            except Exception:
                pass
            parts.append({
                "functionResponse": {
                    "name": tool_name,
                    "response": resp_data
                }
            })

        if parts:
            gemini_role = "model" if role == "assistant" else "user"
            contents.append({
                "role": gemini_role,
                "parts": parts
            })

    return system_instruction, contents

def get_api_key(request: Request) -> str:
    """Extracts API key from Authorization header or environment variable."""
    auth_header = request.headers.get("Authorization")
    if auth_header and auth_header.startswith("Bearer "):
        key = auth_header[7:].strip()
        if key and key != "local":
            return key
    
    # Fallback to env variables
    key = os.environ.get("GEMINI_API_KEY") or os.environ.get("ANTIGRAVITY_API_KEY")
    if not key:
        raise HTTPException(
            status_code=401,
            detail="API Key not configured. Please supply a Bearer token or set GEMINI_API_KEY env variable."
        )
    return key

async def stream_direct_gemini(api_key: str, model_name: str, payload: Dict[str, Any]):
    """Streams response chunks directly from the Gemini API."""
    # Resolve default Gemini model name if generic alias is supplied
    gemini_model = "gemini-2.5-flash"
    model_lower = model_name.lower().replace(" ", "-")
    if "nano-banana-pro" in model_lower:
        gemini_model = "gemini-3-pro-image-preview"
    elif "nano-banana-2" in model_lower:
        gemini_model = "gemini-3.1-flash-image-preview"
    elif "nano-banana" in model_lower:
        gemini_model = "gemini-2.5-flash-image"
    elif "opus" in model_name or "sonnet" in model_name:
        gemini_model = "gemini-2.5-pro"
    elif "gemini" in model_lower:
        gemini_model = model_name
        
    url = f"https://generativelanguage.googleapis.com/v1beta/models/{gemini_model}:streamGenerateContent?key={api_key}"
    headers = {"Content-Type": "application/json"}
    
    req_body = json.dumps(payload).encode("utf-8")
    
    # Run the HTTP request in an executor to avoid blocking the asyncio event loop
    loop = asyncio.get_event_loop()
    
    def perform_request():
        req = urllib.request.Request(url, data=req_body, headers=headers, method="POST")
        return urllib.request.urlopen(req, timeout=60)
        
    try:
        response = await loop.run_in_executor(None, perform_request)
    except urllib.error.HTTPError as e:
        err_msg = e.read().decode("utf-8")
        raise HTTPException(status_code=e.code, detail=f"Gemini API returned error: {err_msg}")
    except Exception as e:
        raise HTTPException(status_code=502, detail=f"Failed to connect to Gemini API: {str(e)}")

    buffer = ""
    chunk_id = f"chatcmpl-{os.urandom(8).hex()}"
    
    while True:
        # Read from response stream non-blockingly
        def read_chunk():
            return response.read(1024)
            
        data = await loop.run_in_executor(None, read_chunk)
        if not data:
            break
            
        buffer += data.decode("utf-8")
        # Try to parse JSON lines or JSON streaming chunks
        # The Gemini streamGenerateContent returns a JSON array over time.
        # We can extract text parts or tool calls using simple substring or JSON parsing
        # Standard format is:
        # [
        #   { "candidates": [ { "content": { "parts": [ { "text": "hello" } ] } } ] }
        # ]
        # Since it is a streamed JSON list, we parse JSON objects from the stream.
        while True:
            buffer = buffer.strip()
            if not buffer:
                break
                
            # If it starts with [ (beginning of array), skip it
            if buffer.startswith("["):
                buffer = buffer[1:].strip()
                continue
            
            # If it starts with , (separator), skip it
            if buffer.startswith(","):
                buffer = buffer[1:].strip()
                continue
                
            if buffer.startswith("]"):
                break
                
            # Find the closing brace of the JSON object
            # A simple bracket matching parser
            brace_count = 0
            in_quote = False
            escape = False
            end_idx = -1
            
            for idx, char in enumerate(buffer):
                if escape:
                    escape = False
                    continue
                if char == "\\":
                    escape = True
                    continue
                if char == '"':
                    in_quote = not in_quote
                    continue
                if not in_quote:
                    if char == "{":
                        brace_count += 1
                    elif char == "}":
                        brace_count -= 1
                        if brace_count == 0:
                            end_idx = idx + 1
                            break
                            
            if end_idx == -1:
                break # Incomplete JSON object
                
            obj_str = buffer[:end_idx]
            buffer = buffer[end_idx:].strip()
            
            try:
                obj = json.loads(obj_str)
                candidates = obj.get("candidates", [])
                if not candidates:
                    continue
                    
                content_obj = candidates[0].get("content", {})
                parts = content_obj.get("parts", [])
                
                # Check for tool call
                for part in parts:
                    if "functionCall" in part:
                        func_call = part["functionCall"]
                        tool_chunk = {
                            "id": chunk_id,
                            "object": "chat.completion.chunk",
                            "created": 1677652288,
                            "model": gemini_model,
                            "choices": [
                                {
                                    "index": 0,
                                    "delta": {
                                        "tool_calls": [
                                            {
                                                "index": 0,
                                                "id": f"call_{func_call.get('name')}",
                                                "type": "function",
                                                "function": {
                                                    "name": func_call.get("name"),
                                                    "arguments": json.dumps(func_call.get("args", {}))
                                                }
                                            }
                                        ]
                                    },
                                    "finish_reason": "tool_calls"
                                }
                            ]
                        }
                        yield f"data: {json.dumps(tool_chunk)}\n\n"
                        yield "data: [DONE]\n\n"
                        return

                    # Check for text
                    if "text" in part:
                        text_val = part["text"]
                        chunk = {
                            "id": chunk_id,
                            "object": "chat.completion.chunk",
                            "created": 1677652288,
                            "model": gemini_model,
                            "choices": [
                                {
                                    "index": 0,
                                    "delta": {"content": text_val},
                                    "finish_reason": None
                                }
                            ]
                        }
                        yield f"data: {json.dumps(chunk)}\n\n"
                        
            except Exception as e:
                print(f"Error parsing chunk: {e}", file=sys.stderr)
                
    # Final stop chunk
    stop_chunk = {
        "id": chunk_id,
        "object": "chat.completion.chunk",
        "created": 1677652288,
        "model": gemini_model,
        "choices": [
            {
                "index": 0,
                "delta": {},
                "finish_reason": "stop"
            }
        ]
    }
    yield f"data: {json.dumps(stop_chunk)}\n\n"
    yield "data: [DONE]\n\n"

async def stream_sdk_agent(model_name: str, payload: Dict[str, Any]):
    """Streams response chunks using the google-antigravity SDK Agent."""
    # Build list of dynamic tools if supplied
    tools_list = []
    tool_calls_result = []

    def make_tool_callback(name):
        async def callback(tool_name, args):
            tool_calls_result.append({
                "name": tool_name,
                "args": args
            })
            return "Tool invocation recorded successfully."
        return callback

    # Parse and register tools dynamically
    for tool_def in payload.get("tools", []):
        func_def = tool_def.get("function", {})
        name = func_def.get("name")
        desc = func_def.get("description", "")
        params = func_def.get("parameters", {})
        
        # Simple dynamic function generator
        fields = {}
        for prop_name, prop_info in params.get("properties", {}).items():
            prop_type = str
            t = prop_info.get("type", "string")
            if t == "integer": prop_type = int
            elif t == "number": prop_type = float
            elif t == "boolean": prop_type = bool
            elif t == "array": prop_type = list
            elif t == "object": prop_type = dict
            
            p_desc = prop_info.get("description", "")
            if prop_name in params.get("required", []):
                fields[prop_name] = (prop_type, Field(description=p_desc))
            else:
                fields[prop_name] = (Optional[prop_type], Field(default=None, description=p_desc))
                
        pydantic_model = create_model(f"{name}_args", **fields)
        
        # Callback wrapper
        async def tool_run(args: pydantic_model):
            args_dict = args.model_dump()
            tool_calls_result.append({
                "name": name,
                "args": args_dict
            })
            return "Recorded"
            
        tool_run.__name__ = name
        tool_run.__doc__ = desc
        tools_list.append(tool_run)

    # Initialize SDK LocalAgentConfig
    system_instruction, contents = map_messages_to_gemini(payload.get("messages", []))
    config = LocalAgentConfig(
        system_instructions=system_instruction or "You are a helpful coding assistant.",
        tools=tools_list
    )

    chunk_id = f"chatcmpl-{os.urandom(8).hex()}"
    
    # Run the Agent chat session
    async with Agent(config) as agent:
        # Feed previous user/model content turns to populate session context if the SDK supports it
        # Otherwise, send the latest user prompt
        latest_prompt = "Hello"
        if contents:
            # Reconstruct the last user message as the active prompt
            for turn in reversed(contents):
                if turn.get("role") == "user":
                    for part in turn.get("parts", []):
                        if "text" in part:
                            latest_prompt = part["text"]
                            break
                    break
        
        response = await agent.chat(latest_prompt)
        
        # Stream the tokens
        async for token in response:
            # If a tool call was intercepted/invoked during chat execution
            if tool_calls_result:
                tc = tool_calls_result[0]
                tool_chunk = {
                    "id": chunk_id,
                    "object": "chat.completion.chunk",
                    "created": 1677652288,
                    "model": model_name,
                    "choices": [
                        {
                            "index": 0,
                            "delta": {
                                "tool_calls": [
                                    {
                                        "index": 0,
                                        "id": f"call_{tc['name']}",
                                        "type": "function",
                                        "function": {
                                            "name": tc["name"],
                                            "arguments": json.dumps(tc["args"])
                                        }
                                    }
                                ]
                            },
                            "finish_reason": "tool_calls"
                        }
                    ]
                }
                yield f"data: {json.dumps(tool_chunk)}\n\n"
                yield "data: [DONE]\n\n"
                return

            chunk = {
                "id": chunk_id,
                "object": "chat.completion.chunk",
                "created": 1677652288,
                "model": model_name,
                "choices": [
                    {
                        "index": 0,
                        "delta": {"content": token},
                        "finish_reason": None
                    }
                ]
            }
            yield f"data: {json.dumps(chunk)}\n\n"

    # Final stop chunk
    stop_chunk = {
        "id": chunk_id,
        "object": "chat.completion.chunk",
        "created": 1677652288,
        "model": model_name,
        "choices": [
            {
                "index": 0,
                "delta": {},
                "finish_reason": "stop"
            }
        ]
    }
    yield f"data: {json.dumps(stop_chunk)}\n\n"
    yield "data: [DONE]\n\n"

@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    """OpenAI-compatible chat completions endpoint."""
    try:
        body = await request.json()
    except Exception:
        raise HTTPException(status_code=400, detail="Invalid JSON body")
        
    api_key = get_api_key(request)
    model_name = body.get("model", "gemini-2.5-flash")
    stream = body.get("stream", False)
    
    # Extract messages and map tools
    messages = body.get("messages", [])
    system_instruction, contents = map_messages_to_gemini(messages)
    
    tools = []
    for tool_def in body.get("tools", []):
        func_def = tool_def.get("function", {})
        tools.append({
            "name": func_def.get("name"),
            "description": func_def.get("description", ""),
            "parameters": uppercase_schema_types(func_def.get("parameters", {}))
        })
        
    # Resolve the Gemini model first to decide thinking level strategy
    gemini_model = "gemini-2.5-flash"
    model_lower = model_name.lower().replace(" ", "-")
    if "nano-banana-pro" in model_lower:
        gemini_model = "gemini-3-pro-image-preview"
    elif "nano-banana-2" in model_lower:
        gemini_model = "gemini-3.1-flash-image-preview"
    elif "nano-banana" in model_lower:
        gemini_model = "gemini-2.5-flash-image"
    elif "opus" in model_name or "sonnet" in model_name:
        gemini_model = "gemini-2.5-pro"
    elif "gemini" in model_lower:
        gemini_model = model_name

    # Set thinking budget / level
    effort = body.get("reasoning_effort", "") or os.environ.get("CLAUDE_CODE_EFFORT_LEVEL", "") or os.environ.get("OPENAI_REASONING_EFFORT", "")
    effort = effort.lower().strip()
    
    thinking_config = None
    if effort and effort != "none":
        is_gemini_3 = "gemini-3" in gemini_model
        if is_gemini_3:
            # Map to thinkingLevel (low, medium, high)
            level = "high"
            if effort in ("low", "minimal"):
                level = "low"
            elif effort == "medium":
                level = "medium"
            thinking_config = {"thinkingLevel": level}
        else:
            # Map to thinkingBudget for Gemini 2.5
            budget = -1  # Default to dynamic / high thinking
            if effort in ("low", "minimal"):
                budget = 1024
            elif effort == "medium":
                budget = 4096
            thinking_config = {"thinkingBudget": budget}

    # Construct Gemini payload
    gemini_payload = {
        "contents": contents,
    }
    if system_instruction:
        gemini_payload["systemInstruction"] = {
            "parts": [{"text": system_instruction}]
        }
    if tools:
        gemini_payload["tools"] = [{"functionDeclarations": tools}]
        
    if thinking_config:
        gemini_payload["generationConfig"] = {
            "thinkingConfig": thinking_config
        }
        
    if stream:
        if HAS_SDK:
            return StreamingResponse(
                stream_sdk_agent(model_name, body),
                media_type="text/event-stream"
            )
        else:
            return StreamingResponse(
                stream_direct_gemini(api_key, model_name, gemini_payload),
                media_type="text/event-stream"
            )
    else:
        # Non-streaming call (gather streamed chunks and return a single OpenAI response object)
        # To reuse code, we simply collect chunks from the streaming generator
        chunks = []
        generator = (
            stream_sdk_agent(model_name, body) if HAS_SDK
            else stream_direct_gemini(api_key, model_name, gemini_payload)
        )
        
        content_text = ""
        tool_calls = []
        finish_reason = "stop"
        
        async for chunk_line in generator:
            if chunk_line.startswith("data: "):
                data_str = chunk_line[6:].strip()
                if data_str == "[DONE]":
                    break
                try:
                    chunk_obj = json.loads(data_str)
                    choices = chunk_obj.get("choices", [])
                    if choices:
                        delta = choices[0].get("delta", {})
                        if "content" in delta:
                            content_text += delta["content"]
                        if "tool_calls" in delta:
                            tool_calls.extend(delta["tool_calls"])
                            finish_reason = "tool_calls"
                except Exception:
                    pass
                    
        # Construct final OpenAI response
        resp_id = f"chatcmpl-{os.urandom(8).hex()}"
        message_dict = {"role": "assistant"}
        if tool_calls:
            message_dict["tool_calls"] = tool_calls
        else:
            message_dict["content"] = content_text
            
        final_response = {
            "id": resp_id,
            "object": "chat.completion",
            "created": 1677652288,
            "model": model_name,
            "choices": [
                {
                    "index": 0,
                    "message": message_dict,
                    "finish_reason": finish_reason
                }
            ],
            "usage": {
                "prompt_tokens": 100,
                "completion_tokens": 50,
                "total_tokens": 150
            }
        }
        return final_response

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Antigravity SDK Gateway Sidecar")
    parser.add_argument("--port", type=int, default=4005, help="Port to run the gateway on")
    args = parser.parse_args()
    
    # Save the gateway PID file so the Go proxy can clean it up
    pid_path = os.path.join(os.getcwd(), ".antigravity-gateway.pid")
    with open(pid_path, "w") as f:
        f.write(str(os.getpid()))
        
    print(f"Starting Antigravity SDK Gateway on 127.0.0.1:{args.port} (PID {os.getpid()})")
    uvicorn.run(app, host="127.0.0.1", port=args.port, log_level="info")

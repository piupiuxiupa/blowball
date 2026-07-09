package executor

import "encoding/json"

var schemaBash = json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Shell command to execute inside the sandboxed workspace."
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`)

var schemaPython = json.RawMessage(`{
  "type": "object",
  "properties": {
    "code": {
      "type": "string",
      "description": "Python code to run inline via python3 -c."
    },
    "file": {
      "type": "string",
      "description": "Relative path to a Python file inside the workspace to execute."
    }
  },
  "oneOf": [
    { "required": ["code"] },
    { "required": ["file"] }
  ],
  "additionalProperties": false
}`)

var schemaPip = json.RawMessage(`{
  "type": "object",
  "properties": {
    "packages": {
      "type": "array",
      "description": "Python packages to install via pip (e.g. [\"requests\", \"numpy>=2.0\"]).",
      "items": { "type": "string" },
      "minItems": 1
    },
    "upgrade": {
      "type": "boolean",
      "description": "When true, pass --upgrade to install the latest versions."
    }
  },
  "required": ["packages"],
  "additionalProperties": false
}`)

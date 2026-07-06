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

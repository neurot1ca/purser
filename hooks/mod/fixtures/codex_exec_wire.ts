export const CODEX_EXEC_WIRE_FIXTURES = [
  {
    "name": "job_result_alias_unicode",
    "input": {
      "function": "_job_result",
      "value": {
        "conversationId": "oturum-ıd 🪁",
        "threadId": "işlem-çubuğu",
        "message": "Başarılı ✅"
      }
    },
    "expected_text": "{\"conversationId\": \"oturum-ıd 🪁\", \"threadId\": \"işlem-çubuğu\", \"message\": \"Başarılı ✅\"}"
  },
  {
    "name": "job_result_error",
    "input": {
      "function": "_job_result",
      "value": {
        "state": "FAILED",
        "errorCode": "INVALID_ARGUMENT",
        "message": "bad request"
      }
    },
    "expected_text": "{\"state\": \"FAILED\", \"errorCode\": \"INVALID_ARGUMENT\", \"message\": \"bad request\"}"
  },
  {
    "name": "job_result_success",
    "input": {
      "function": "_job_result",
      "value": {
        "jobId": "job-123",
        "state": "SUCCEEDED",
        "content": "done"
      }
    },
    "expected_text": "{\"jobId\": \"job-123\", \"state\": \"SUCCEEDED\", \"content\": \"done\"}"
  },
  {
    "name": "schema_codex",
    "input": {
      "function": "schema",
      "tool": "codex"
    },
    "expected_text": "{\"name\": \"codex\", \"description\": \"Run a new Codex thread.\", \"inputSchema\": {\"type\": \"object\", \"properties\": {\"prompt\": {\"type\": \"string\"}, \"label\": {\"type\": \"string\", \"description\": \"Short human task description in the user's language (up to 80 characters).\"}, \"model\": {\"type\": \"string\"}, \"cwd\": {\"type\": \"string\"}, \"config\": {\"type\": \"object\"}, \"developer-instructions\": {\"type\": \"string\"}, \"base-instructions\": {\"type\": \"string\"}, \"compact-prompt\": {\"type\": \"string\"}, \"sandbox\": {\"type\": \"string\", \"enum\": [\"read-only\", \"workspace-write\", \"danger-full-access\"]}, \"approval-policy\": {\"type\": \"string\", \"enum\": [\"on-request\", \"never\"]}}, \"required\": [\"prompt\"]}}"
  },
  {
    "name": "schema_codex_apply",
    "input": {
      "function": "schema",
      "tool": "codex_apply"
    },
    "expected_text": "{\"name\": \"codex_apply\", \"description\": \"Apply a job worktree patch.\", \"inputSchema\": {\"type\": \"object\", \"properties\": {\"jobId\": {\"type\": \"string\"}, \"resolved\": {\"type\": \"boolean\", \"description\": \"Record a hand-resolved conflict after verifying the repository.\"}, \"reviewer1Valid\": {\"type\": \"integer\", \"minimum\": 0}, \"reviewer1False\": {\"type\": \"integer\", \"minimum\": 0}, \"acceptOnly\": {\"type\": \"boolean\"}, \"sessionId\": {\"type\": \"string\"}, \"revision\": {\"type\": \"string\"}}, \"required\": [\"jobId\"]}}"
  },
  {
    "name": "schema_codex_cancel",
    "input": {
      "function": "schema",
      "tool": "codex_cancel"
    },
    "expected_text": "{\"name\": \"codex_cancel\", \"description\": \"Cancel a durable Codex job.\", \"inputSchema\": {\"type\": \"object\", \"properties\": {\"jobId\": {\"type\": \"string\"}, \"reason\": {\"type\": \"string\"}}, \"required\": [\"jobId\"]}}"
  },
  {
    "name": "schema_codex_reply_alias",
    "input": {
      "function": "schema",
      "tool": "codex-reply"
    },
    "expected_text": "{\"name\": \"codex-reply\", \"description\": \"Continue a thread created by this server.\", \"inputSchema\": {\"type\": \"object\", \"properties\": {\"threadId\": {\"type\": \"string\"}, \"conversationId\": {\"type\": \"string\", \"description\": \"Deprecated alias for threadId.\"}, \"prompt\": {\"type\": \"string\"}, \"label\": {\"type\": \"string\", \"description\": \"Short human task description in the user's language (up to 80 characters); may be omitted to reuse the thread label.\"}}, \"required\": [\"prompt\"]}}"
  },
  {
    "name": "schema_codex_result",
    "input": {
      "function": "schema",
      "tool": "codex_result"
    },
    "expected_text": "{\"name\": \"codex_result\", \"description\": \"Read a durable Codex job.\", \"inputSchema\": {\"type\": \"object\", \"properties\": {\"jobId\": {\"type\": \"string\"}, \"adopt\": {\"type\": \"boolean\", \"description\": \"After user consent, finalize a job from an earlier session in this project.\"}}, \"required\": [\"jobId\"]}}"
  },
  {
    "name": "schema_codex_start",
    "input": {
      "function": "schema",
      "tool": "codex_start"
    },
    "expected_text": "{\"name\": \"codex_start\", \"description\": \"Start a durable Codex job.\", \"inputSchema\": {\"type\": \"object\", \"properties\": {\"prompt\": {\"type\": \"string\"}, \"label\": {\"type\": \"string\", \"description\": \"Short human task description in the user's language (up to 80 characters).\"}, \"model\": {\"type\": \"string\"}, \"cwd\": {\"type\": \"string\"}, \"config\": {\"type\": \"object\"}, \"developer-instructions\": {\"type\": \"string\"}, \"base-instructions\": {\"type\": \"string\"}, \"compact-prompt\": {\"type\": \"string\"}, \"sandbox\": {\"type\": \"string\", \"enum\": [\"read-only\", \"workspace-write\", \"danger-full-access\"]}, \"approval-policy\": {\"type\": \"string\", \"enum\": [\"on-request\", \"never\"]}, \"call\": {\"type\": \"string\"}, \"mode\": {\"type\": \"string\", \"enum\": [\"new\", \"reply\"]}, \"worktree\": {\"type\": \"boolean\"}, \"threadId\": {\"type\": \"string\"}, \"timeoutSeconds\": {\"type\": \"number\"}, \"taskId\": {\"type\": \"string\"}, \"route\": {\"type\": \"string\"}, \"etaMinutes\": {\"type\": \"integer\"}, \"jobModel\": {\"type\": \"string\"}, \"jobEffort\": {\"type\": \"string\"}, \"jobLabel\": {\"type\": \"string\"}, \"jobPhase\": {\"type\": \"string\"}, \"readinessCriteria\": {\"type\": \"object\"}, \"readinessGrounded\": {\"type\": \"boolean\"}, \"readinessAgent\": {\"type\": \"string\"}, \"readinessModel\": {\"type\": \"string\"}}, \"required\": [\"prompt\"]}}"
  },
  {
    "name": "tool_result_alias_unicode",
    "input": {
      "function": "_tool_result",
      "thread_id": "iş parçacığı 🧵",
      "content": "Merhaba, dünya! 🚀"
    },
    "expected_text": "{\"threadId\": \"iş parçacığı 🧵\", \"content\": \"Merhaba, dünya! 🚀\"}"
  },
  {
    "name": "tool_result_error",
    "input": {
      "function": "_tool_result",
      "thread_id": null,
      "error": "request failed"
    },
    "expected_text": "{\"threadId\": null, \"error\": \"request failed\"}"
  },
  {
    "name": "tool_result_success",
    "input": {
      "function": "_tool_result",
      "thread_id": "thread-123",
      "content": "completed"
    },
    "expected_text": "{\"threadId\": \"thread-123\", \"content\": \"completed\"}"
  },
  {
    "name": "tool_result_usage_absent",
    "input": {
      "function": "_tool_result",
      "thread_id": "thread-usage",
      "content": "done",
      "usage": {}
    },
    "expected_text": "{\"threadId\": \"thread-usage\", \"content\": \"done\"}"
  },
  {
    "name": "tool_result_usage_present",
    "input": {
      "function": "_tool_result",
      "thread_id": "thread-usage",
      "content": "done",
      "usage": {
        "input_tokens": 12,
        "output_tokens": 3
      }
    },
    "expected_text": "{\"threadId\": \"thread-usage\", \"content\": \"done\", \"usage\": {\"input_tokens\": 12, \"output_tokens\": 3}}"
  }
] as const

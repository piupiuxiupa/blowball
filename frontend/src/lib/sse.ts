export interface SSEEvent {
  event: string;
  data: string;
}

export async function* parseSSEStream(response: Response): AsyncGenerator<SSEEvent> {
  if (!response.body) {
    return;
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });

      let boundary = buffer.indexOf('\n\n');
      while (boundary !== -1) {
        const chunk = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        const parsed = parseEvent(chunk);
        if (parsed) yield parsed;
        boundary = buffer.indexOf('\n\n');
      }
    }

    // Flush any remaining content
    const remaining = decoder.decode();
    if (remaining) {
      buffer += remaining;
      let boundary = buffer.indexOf('\n\n');
      while (boundary !== -1) {
        const chunk = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        const parsed = parseEvent(chunk);
        if (parsed) yield parsed;
        boundary = buffer.indexOf('\n\n');
      }
    }
  } finally {
    reader.releaseLock();
  }
}

function parseEvent(chunk: string): SSEEvent | null {
  const lines = chunk.split('\n');
  let event = '';
  let data = '';

  for (const line of lines) {
    if (line.startsWith('event: ')) {
      event = line.slice(7).trim();
    } else if (line.startsWith('data: ')) {
      data = line.slice(6);
    }
  }

  if (!event) return null;
  return { event, data };
}

// Typed client for the ferrite daemon's HTTP API.
//
// Mirrors cmd/ferrite-tui/client.go — same endpoints, same reasoning: the
// daemon is the single source of truth about the TV, so nothing here caches
// state.

export interface Channel {
  name: string;
  aliases?: string[];
  service_id: number;
}

export interface Adapter {
  adapter: number;
  channel?: string;
  refs: number;
  prio?: string;
  reserved?: boolean;
}

export interface Status {
  version: string;
  uptime: string;
  adapters: Adapter[];
  recording?: number[];
}

export interface Event {
  service_id: number;
  event_id: number;
  start: string; // RFC3339
  duration_s: number;
  title: string;
  synopsis?: string;
}

export interface Recording {
  id: number;
  channel: string;
  title?: string;
  start: string;
  end: string | null;
  path: string;
  size_bytes: number | null;
  state: "recording" | "done" | "failed";
  error?: string;
}

export interface Schedule {
  id: number;
  channel: string;
  service_id: number;
  start: string;
  end: string;
  state: string;
}

export interface SwitchResult {
  channel: string;
  playlist: string;
  closed: string[] | null;
}

export interface RecordResult {
  id: number;
  channel: string;
  title: string;
}

/** An error carrying the daemon's own message, which is the actionable part. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }

  /** The tuner is occupied — worth explaining rather than just reporting. */
  get busy(): boolean {
    return this.status === 409;
  }
}

export class FerriteClient {
  readonly baseUrl: string;

  constructor(
    baseUrl = process.env.FERRITE_HOST ?? "http://localhost:8010",
    // A cold channel change runs the frontend lock plus ffmpeg's first
    // segment, so a switch legitimately takes several seconds.
    private readonly timeoutMs = 90_000,
  ) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
  }

  channels(): Promise<Channel[]> {
    return this.getList<Channel>("/api/channels");
  }

  status(): Promise<Status> {
    return this.get<Status>("/api/status");
  }

  now(serviceId: number): Promise<Event | null> {
    return this.get<Event | null>(`/api/now?service=${serviceId}`);
  }

  schedule(serviceId: number, from: Date, to: Date): Promise<Event[]> {
    const q = new URLSearchParams({
      service: String(serviceId),
      from: from.toISOString(),
      to: to.toISOString(),
    });
    return this.getList<Event>(`/api/epg?${q}`);
  }

  switchTo(channel: string): Promise<SwitchResult> {
    return this.post<SwitchResult>(`/api/live/${encodeURIComponent(channel)}/switch`);
  }

  stopLive(channel: string): Promise<void> {
    return this.post<void>(`/api/live/${encodeURIComponent(channel)}/stop`);
  }

  record(channel: string, title?: string, durationS?: number): Promise<RecordResult> {
    const body: Record<string, unknown> = { channel };
    if (title) body.title = title;
    if (durationS && durationS > 0) body.duration_s = Math.round(durationS);
    return this.post<RecordResult>("/api/record", body);
  }

  stopRecording(id: number): Promise<void> {
    return this.post<void>(`/api/record/${id}/stop`);
  }

  recordings(): Promise<Recording[]> {
    return this.getList<Recording>("/api/recordings");
  }

  schedules(): Promise<Schedule[]> {
    return this.getList<Schedule>("/api/schedule");
  }

  createSchedule(input: {
    channel: string;
    service_id: number;
    start: string;
    end: string;
    lead_s?: number;
    trail_s?: number;
  }): Promise<{ id: number }> {
    return this.post<{ id: number }>("/api/schedule", input);
  }

  cancelSchedule(id: number): Promise<void> {
    return this.request<void>("DELETE", `/api/schedule/${id}`);
  }

  /** Absolute playlist URL, for handing to a player. */
  playlistUrl(channel: string): string {
    return `${this.baseUrl}/api/live/${encodeURIComponent(channel)}.m3u8`;
  }

  private get<T>(path: string): Promise<T> {
    return this.request<T>("GET", path);
  }

  /**
   * GET a list, tolerating `null`.
   *
   * A Go nil slice marshals as `null`, not `[]`, so an empty guide or an
   * empty recording list arrives as null and `.map` on it throws. The daemon
   * now normalizes this, but a client that crashes when a server regresses is
   * a bad client.
   */
  private async getList<T>(path: string): Promise<T[]> {
    return (await this.request<T[] | null>("GET", path)) ?? [];
  }

  private post<T>(path: string, body?: unknown): Promise<T> {
    return this.request<T>("POST", path, body);
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const signal = AbortSignal.timeout(this.timeoutMs);
    let res: Response;
    try {
      res = await fetch(this.baseUrl + path, {
        method,
        signal,
        headers: body === undefined ? undefined : { "content-type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err);
      throw new Error(`${method} ${path}: ${reason}`);
    }

    if (!res.ok) {
      throw new ApiError(res.status, await errorMessage(res));
    }
    if (res.status === 204) return undefined as T;

    const text = await res.text();
    if (!text) return undefined as T;
    return JSON.parse(text) as T;
  }
}

async function errorMessage(res: Response): Promise<string> {
  const text = await res.text().catch(() => "");
  try {
    const parsed = JSON.parse(text) as { error?: string };
    if (parsed.error) return parsed.error;
  } catch {
    // Not JSON — a proxy error page, say. Keep the body.
  }
  return text.trim() || `HTTP ${res.status}`;
}

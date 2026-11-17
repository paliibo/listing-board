import { FormEvent, useEffect, useRef, useState } from "react";

type Step = { label: string; state: string };

type Job = {
  id: string;
  title: string;
  address: string;
  status: string;
  error: string;
  runId: string;
  filename: string;
  checklist: Step[];
  createdAt: string;
};

type Account = { id: string; email: string };

const MAX_BROCHURE = 1048576;
const TOO_LARGE = "Brochure is too large";
// How often the list is fetched: while a sheet is running, and otherwise.
const POLL_RUNNING = 1000;
const POLL_IDLE = 3000;

class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: "include", ...init });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    const body = data as { error?: unknown } | null;
    const message =
      body && typeof body.error === "string" && body.error
        ? body.error
        : res.statusText || "Request failed (" + res.status + ")";
    throw new ApiError(message, res.status);
  }
  return data as T;
}

function sleep(ms: number) {
  return new Promise<void>((resolve) => setTimeout(resolve, ms));
}

function doneSteps(job: Job) {
  return job.checklist.filter((step) => step.state === "done").length;
}

// One sentence about a sheet as it is now, for a reader who cannot see the checklist move.
function describe(job: Job) {
  const done = job.checklist.filter((step) => step.state === "done");
  if (job.status === "failed") return job.title + ": failed. " + job.error;
  if (job.status === "running" && done.length > 0) {
    return (
      job.title + ": " + done[done.length - 1].label + " is done, " + done.length + " of " + job.checklist.length
    );
  }
  return job.title + ": " + job.status;
}

export function App() {
  const [me, setMe] = useState<Account | null>(null);
  const [ready, setReady] = useState(false);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [error, setError] = useState("");
  // What a screen reader is told; the text lives in one polite live region.
  const [notice, setNotice] = useState("");
  // The open sheet as it was last seen: its id, status and number of steps done.
  const seen = useRef("");
  // Bumped by every local change to the list; a list answer requested before a bump is stale.
  const version = useRef(0);
  // Bumped whenever the user opens a sheet; Create compares it to decide whether to open the new sheet.
  const openStamp = useRef(0);
  // True while a list request is in flight, so list requests never overlap.
  const inFlight = useRef(false);

  const selected = jobs.find((job) => job.id === selectedId) ?? null;
  const anyRunning = jobs.some((job) => job.status === "running");
  const state = selected ? selected.id + " " + selected.status + " " + doneSteps(selected) : "";

  async function requestList(cancelled: () => boolean, quiet: boolean): Promise<"ok" | "stale" | "stop" | "error"> {
    inFlight.current = true;
    const seen = version.current;
    try {
      const list = await api<Job[]>("/api/jobs");
      if (cancelled()) return "stop";
      if (seen !== version.current) return "stale";
      setJobs(list);
      return "ok";
    } catch (err) {
      if (cancelled()) return "stop";
      if (err instanceof ApiError && err.status === 401) {
        setMe(null);
        return "stop";
      }
      if (!quiet) setError(err instanceof Error ? err.message : "Could not load the sheets");
      return "error";
    } finally {
      inFlight.current = false;
    }
  }

  function merge(job: Job) {
    version.current += 1;
    setJobs((current) =>
      current.some((item) => item.id === job.id)
        ? current.map((item) => (item.id === job.id ? job : item))
        : [job, ...current],
    );
  }

  function open(id: string) {
    openStamp.current += 1;
    setSelectedId(id);
  }

  useEffect(() => {
    api<Account>("/api/me")
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setReady(true));
  }, []);

  // First list load after sign-in; asks again when its answer was thrown away.
  useEffect(() => {
    if (!me) {
      setJobs([]);
      return;
    }
    let cancelled = false;
    void (async () => {
      for (;;) {
        while (inFlight.current && !cancelled) await sleep(100);
        if (cancelled) return;
        const result = await requestList(() => cancelled, false);
        if (result !== "stale") return;
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [me]);

  // Keep the list fresh: one request at a time, the next one scheduled after the previous finished.
  // It also runs, more slowly, when nothing is running here: a sheet can be started in another tab,
  // or the answer to Start can get lost, and the page must still follow the worker without a reload.
  useEffect(() => {
    if (!me) return;
    const wait = anyRunning ? POLL_RUNNING : POLL_IDLE;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async () => {
      if (!inFlight.current) {
        const result = await requestList(() => cancelled, true);
        if (result === "stop") return;
      }
      if (!cancelled) timer = setTimeout(tick, wait);
    };
    timer = setTimeout(tick, wait);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [me, anyRunning]);

  // The checklist moves without the reader doing anything, so a change on the open sheet is
  // announced. Opening a sheet is not a change, and neither is the first list after sign-in.
  useEffect(() => {
    const before = seen.current;
    seen.current = state;
    if (selected && before !== state && before.startsWith(selected.id + " ")) {
      setNotice(describe(selected));
    }
  }, [state]);

  if (!ready) {
    return <p className="muted loading">Loading…</p>;
  }
  if (!me) {
    return <Auth onIn={setMe} />;
  }

  return (
    <main>
      <header className="top">
        <div>
          <p className="eyebrow">Real estate</p>
          <h1>Listing board</h1>
        </div>
        <div className="account">
          <span className="muted">{me.email}</span>
          <button
            type="button"
            className="ghost"
            onClick={() => {
              api("/api/logout", { method: "POST" }).finally(() => {
                setMe(null);
                setSelectedId(null);
                setError("");
                setNotice("");
              });
            }}
          >
            Log out
          </button>
        </div>
      </header>
      <p className="visually-hidden" role="status">
        {notice}
      </p>
      {error ? (
        <div className="error banner">
          <span role="alert">{error}</span>
          <button type="button" className="ghost" onClick={() => setError("")}>
            Dismiss
          </button>
        </div>
      ) : null}
      <section className="panel" aria-labelledby="new-sheet">
        <h2 className="label" id="new-sheet">
          New sheet
        </h2>
        <Create
          getStamp={() => openStamp.current}
          onCreated={(job, stamp) => {
            merge(job);
            if (stamp === openStamp.current) setSelectedId(job.id);
          }}
          onError={setError}
        />
      </section>
      <div className="layout">
        <section className="sheets" aria-labelledby="sheets">
          <h2 className="label" id="sheets">
            Sheets
          </h2>
          <ul className="list">
            {jobs.length === 0 ? <li className="muted">No sheets yet.</li> : null}
            {jobs.map((job) => (
              <li key={job.id}>
                <button
                  type="button"
                  className={selectedId === job.id ? "row on" : "row"}
                  aria-current={selectedId === job.id ? "true" : undefined}
                  onClick={() => open(job.id)}
                >
                  <span className="row-main">
                    <strong>{job.title}</strong>
                    <span className="muted">{job.address}</span>
                  </span>
                  <span className="row-side">
                    {job.status === "running" ? (
                      <span className="count">
                        {doneSteps(job)}/{job.checklist.length}
                      </span>
                    ) : null}
                    <span className={"status " + job.status}>{job.status}</span>
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </section>
        {selected ? (
          <Sheet key={selected.id} job={selected} onChange={merge} onError={setError} />
        ) : (
          <p className="muted empty">Select a sheet.</p>
        )}
      </div>
    </main>
  );
}

function Auth({ onIn }: { onIn: (account: Account) => void }) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    try {
      const account = await api<Account>("/api/" + mode, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, password }),
      });
      onIn(account);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not sign in");
    }
  }

  return (
    <main className="narrow">
      <div>
        <p className="eyebrow">Real estate</p>
        <h1>Listing board</h1>
      </div>
      <form className="panel" onSubmit={submit}>
        <label>
          Email
          <input
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            type="email"
            name="email"
            autoComplete="username"
            autoCapitalize="none"
            spellCheck={false}
            required
          />
        </label>
        <label>
          Password
          <input
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            type="password"
            name="password"
            autoComplete={mode === "login" ? "current-password" : "new-password"}
            required
          />
        </label>
        {error ? (
          <p className="error" role="alert">
            {error}
          </p>
        ) : null}
        <button type="submit">{mode === "login" ? "Sign in" : "Create account"}</button>
      </form>
      <button type="button" className="ghost" onClick={() => setMode(mode === "login" ? "register" : "login")}>
        {mode === "login" ? "Need an account?" : "Already have an account?"}
      </button>
    </main>
  );
}

function Create({
  getStamp,
  onCreated,
  onError,
}: {
  getStamp: () => number;
  onCreated: (job: Job, stamp: number) => void;
  onError: (message: string) => void;
}) {
  const [title, setTitle] = useState("");
  const [address, setAddress] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [fileKey, setFileKey] = useState(0);
  // A second click while the two requests are on their way would create a second sheet.
  const [busy, setBusy] = useState(false);

  function reset() {
    setTitle("");
    setAddress("");
    setFile(null);
    setFileKey((key) => key + 1);
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!file) {
      onError("Choose a brochure file");
      return;
    }
    if (file.size > MAX_BROCHURE) {
      onError(TOO_LARGE);
      return;
    }
    const stamp = getStamp();
    let created: Job | null = null;
    setBusy(true);
    try {
      created = await api<Job>("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title, address }),
      });
      const body = new FormData();
      body.append("file", file);
      const saved = await api<Job>("/api/jobs/" + created.id + "/file", { method: "POST", body });
      reset();
      onError("");
      onCreated(saved, stamp);
    } catch (err) {
      const message = err instanceof Error ? err.message : "Could not create the sheet";
      if (created) {
        // The draft exists on the server: show it, then the upload error.
        reset();
        onCreated(created, stamp);
      }
      onError(message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="create" onSubmit={submit}>
      <label>
        Title
        <input value={title} onChange={(event) => setTitle(event.target.value)} required />
      </label>
      <label>
        Address
        <input value={address} onChange={(event) => setAddress(event.target.value)} required />
      </label>
      <label>
        <span>
          Brochure <span className="hint">PDF or plain text, up to 1 MiB</span>
        </span>
        <input
          key={fileKey}
          type="file"
          onChange={(event) => setFile(event.target.files?.[0] ?? null)}
          required
        />
      </label>
      <button type="submit" disabled={busy}>
        Create sheet
      </button>
    </form>
  );
}

function Sheet({
  job,
  onChange,
  onError,
}: {
  job: Job;
  onChange: (job: Job) => void;
  onError: (message: string) => void;
}) {
  const [file, setFile] = useState<File | null>(null);
  const [fileKey, setFileKey] = useState(0);
  // A second upload of the same file would be answered with "already has a brochure".
  const [busy, setBusy] = useState(false);

  async function start() {
    setBusy(true);
    try {
      const next = await api<Job>("/api/jobs/" + job.id + "/start", { method: "POST" });
      onChange(next);
      onError("");
    } catch (err) {
      onError(err instanceof Error ? err.message : "Could not start the sheet");
    } finally {
      setBusy(false);
    }
  }

  async function attach(event: FormEvent) {
    event.preventDefault();
    if (!file) {
      onError("Choose a brochure file");
      return;
    }
    if (file.size > MAX_BROCHURE) {
      onError(TOO_LARGE);
      return;
    }
    setBusy(true);
    try {
      const body = new FormData();
      body.append("file", file);
      const saved = await api<Job>("/api/jobs/" + job.id + "/file", { method: "POST", body });
      setFile(null);
      setFileKey((key) => key + 1);
      onChange(saved);
      onError("");
    } catch (err) {
      onError(err instanceof Error ? err.message : "Could not attach the brochure");
    } finally {
      setBusy(false);
    }
  }

  // The step the worker is on: the first one not done yet, while the sheet runs.
  const current = job.status === "running" ? job.checklist.findIndex((step) => step.state !== "done") : -1;

  return (
    <article className="sheet">
      <header className="sheet-head">
        <div>
          <h2>{job.title}</h2>
          <p className="muted">{job.address}</p>
        </div>
        <p className={"status " + job.status}>{job.status}</p>
      </header>
      {job.error ? <p className="error">{job.error}</p> : null}
      <div className="checklist">
        <p className="label">
          Checklist
          {job.status === "draft" ? null : (
            <span className="count">
              {doneSteps(job)} of {job.checklist.length}
            </span>
          )}
        </p>
        <ol className="steps">
          {job.checklist.map((step, index) => (
            <li key={step.label} className={step.state} aria-current={index === current ? "step" : undefined}>
              {step.label}
              <span className="visually-hidden">, {step.state}</span>
            </li>
          ))}
        </ol>
      </div>
      {job.filename || job.runId ? (
        <dl className="meta">
          {job.filename ? (
            <>
              <dt>Brochure</dt>
              <dd>
                <a href={"/api/jobs/" + job.id + "/file"}>{job.filename}</a>
              </dd>
            </>
          ) : null}
          {job.runId ? (
            <>
              <dt>Run</dt>
              <dd>
                <code>{job.runId}</code>
              </dd>
            </>
          ) : null}
        </dl>
      ) : null}
      {job.status === "draft" && !job.filename ? (
        <form onSubmit={attach}>
          <label>
            <span>
              Brochure <span className="hint">PDF or plain text, up to 1 MiB</span>
            </span>
            <input
              key={fileKey}
              type="file"
              onChange={(event) => setFile(event.target.files?.[0] ?? null)}
              required
            />
          </label>
          <button type="submit" disabled={busy}>
            Attach brochure
          </button>
        </form>
      ) : null}
      {job.status === "draft" && job.filename ? (
        <button type="button" onClick={start} disabled={busy}>
          Start
        </button>
      ) : null}
    </article>
  );
}

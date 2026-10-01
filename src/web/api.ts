import type {
  AppState,
  AttachDatapackRequest,
  AuthStatus,
  ChangeSide,
  CreateProjectRequest,
  CreateSessionRequest,
  CloneRepoRequest,
  DatapackPeople,
  DesignDoc,
  DiscardCheck,
  InviteCheck,
  LoginRequest,
  MekoDatapack,
  MoveSessionRequest,
  NewComment,
  Project,
  ProjectHome,
  UsageOverview,
  RepoBranches,
  ReposResponse,
  PrReviewers,
  RepoInfo,
  ReviewablePrs,
  ReviewComment,
  SearchResult,
  SendCommentsResult,
  Session,
  SessionAction,
  SessionChanges,
  Settings,
  SettingsUpdate,
  SetDedicatedDatapackRequest,
  SetupRequest,
  ShareDatapackRequest,
  TotpEnrollResponse,
  UpdateProjectRequest,
  UpdateStatus,
} from '../shared/types';

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public body: Record<string, unknown> = {},
  ) {
    super(message);
  }
}

/** Called when any request comes back 401, so the app can drop to the login screen. */
let onUnauthorized: (() => void) | null = null;
export function setUnauthorizedHandler(fn: (() => void) | null) {
  onUnauthorized = fn;
}

async function request<T>(method: string, path: string, body?: unknown, opts: { quiet401?: boolean } = {}): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      credentials: 'same-origin',
      headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new ApiError(0, 'Can’t reach the AgentK Local server. Check that it is running.');
  }
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let data: unknown = undefined;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { error: text };
    }
  }
  if (!res.ok) {
    const obj = (data && typeof data === 'object' ? data : {}) as Record<string, unknown>;
    const msg =
      typeof obj.error === 'string'
        ? obj.error
        : res.status === 429
          ? 'Too many attempts. Wait a few minutes and try again.'
          : `Request failed (${res.status}).`;
    if (res.status === 401 && !opts.quiet401) onUnauthorized?.();
    throw new ApiError(res.status, msg, obj);
  }
  return data as T;
}

const get = <T>(p: string) => request<T>('GET', p);
const post = <T>(p: string, body?: unknown) => request<T>('POST', p, body ?? {});
const enc = encodeURIComponent;

export const api = {
  authStatus: () => request<AuthStatus>('GET', '/api/auth/status', undefined, { quiet401: true }),
  setup: (b: SetupRequest) => request<void>('POST', '/api/auth/setup', b, { quiet401: true }),
  login: (b: LoginRequest) => request<void>('POST', '/api/auth/login', b, { quiet401: true }),
  logout: () => post<void>('/api/auth/logout'),
  changePassword: (current: string, next: string) => post<void>('/api/auth/password', { current, next }),
  totpEnroll: () => post<TotpEnrollResponse>('/api/auth/totp/enroll'),
  totpConfirm: (code: string) => post<void>('/api/auth/totp/confirm', { code }),
  totpDisable: (code: string) => post<void>('/api/auth/totp/disable', { code }),

  state: () => get<AppState>('/api/state'),
  repos: () => get<ReposResponse>('/api/repos'),
  repoBranches: (path: string) => get<RepoBranches>(`/api/repos/branches?path=${enc(path)}`),
  cloneRepo: (b: CloneRepoRequest) => post<RepoInfo>('/api/repos/clone', b),
  datapacks: () => get<MekoDatapack[]>('/api/meko/datapacks'),
  datapackPeople: (id: string) => get<DatapackPeople>(`/api/meko/datapacks/${enc(id)}/people`),
  shareDatapack: (id: string, b: ShareDatapackRequest) => post<DatapackPeople>(`/api/meko/datapacks/${enc(id)}/people`, b),
  resendDatapackInvite: (id: string, inviteId: string) =>
    post<DatapackPeople>(`/api/meko/datapacks/${enc(id)}/people/invites/${enc(inviteId)}/resend`),
  revokeDatapackInvite: (id: string, inviteId: string) =>
    request<DatapackPeople>('DELETE', `/api/meko/datapacks/${enc(id)}/people/invites/${enc(inviteId)}`),
  checkInvites: () => post<InviteCheck>('/api/invites/check'),
  acceptInvite: (datapackId: string) => post<void>(`/api/invites/${enc(datapackId)}/accept`),
  declineInvite: (datapackId: string) => post<void>(`/api/invites/${enc(datapackId)}/decline`),
  laterInvite: (datapackId: string) => post<void>(`/api/invites/${enc(datapackId)}/later`),

  openProject: (b: CreateProjectRequest) => post<Project>('/api/projects', b),
  updateProject: (id: string, b: UpdateProjectRequest) => request<Project>('PATCH', `/api/projects/${enc(id)}`, b),
  closeProject: (id: string) => request<void>('DELETE', `/api/projects/${enc(id)}`),
  branches: (id: string) => get<string[]>(`/api/projects/${enc(id)}/branches`),
  attachDatapack: (id: string, b: AttachDatapackRequest) => post<Project>(`/api/projects/${enc(id)}/datapacks`, b),
  setDedicatedDatapack: (id: string, b: SetDedicatedDatapackRequest) => post<Project>(`/api/projects/${enc(id)}/dedicated-datapack`, b),
  detachDatapack: (id: string, dpId: string) =>
    request<Project>('DELETE', `/api/projects/${enc(id)}/datapacks/${enc(dpId)}`),
  home: (id: string) => get<ProjectHome>(`/api/projects/${enc(id)}/home`),
  prReviewers: (id: string) => get<PrReviewers[]>(`/api/projects/${enc(id)}/pr-reviewers`),
  /** The project's PRs to review, or with `pr` the one PR typed (a bare number in `repoPath`). */
  reviewPrs: (id: string, q: { pr?: string; repoPath?: string } = {}) =>
    get<ReviewablePrs>(`/api/projects/${enc(id)}/review-prs${q.pr ? `?pr=${enc(q.pr)}${q.repoPath ? `&repoPath=${enc(q.repoPath)}` : ''}` : ''}`),
  usage: () => get<UsageOverview>('/api/usage'),
  search: (id: string, query: string) => post<SearchResult[]>(`/api/projects/${enc(id)}/search`, { query }),
  createSession: (projectId: string, b: CreateSessionRequest) =>
    post<Session>(`/api/projects/${enc(projectId)}/sessions`, b),
  sessionAction: (id: string, action: SessionAction) => post<Session>(`/api/sessions/${enc(id)}/actions/${action}`),
  moveSession: (id: string, b: MoveSessionRequest) => post<Session>(`/api/sessions/${enc(id)}/move`, b),
  changes: (id: string) => get<SessionChanges>(`/api/sessions/${enc(id)}/changes`),
  /** URL of one side of a changed image; `v` busts the cache when the diff is refetched. */
  changeImageUrl: (id: string, path: string, side: ChangeSide, v: number) =>
    `/api/sessions/${enc(id)}/changes/image?path=${enc(path)}&side=${side}&v=${v}`,
  comments: (id: string) => get<ReviewComment[]>(`/api/sessions/${enc(id)}/comments`),
  addComment: (id: string, b: NewComment) => post<ReviewComment>(`/api/sessions/${enc(id)}/comments`, b),
  editComment: (id: string, cid: string, body: string) =>
    request<ReviewComment>('PATCH', `/api/sessions/${enc(id)}/comments/${enc(cid)}`, { body }),
  deleteComment: (id: string, cid: string) => request<void>('DELETE', `/api/sessions/${enc(id)}/comments/${enc(cid)}`),
  sendComments: (id: string) => post<SendCommentsResult>(`/api/sessions/${enc(id)}/comments/send`),
  design: (id: string) => get<DesignDoc>(`/api/sessions/${enc(id)}/design`),
  discardCheck: (id: string) => get<DiscardCheck>(`/api/sessions/${enc(id)}/discard-check`),

  settings: () => get<Settings>('/api/settings'),
  saveSettings: (b: SettingsUpdate) => request<Settings>('PUT', '/api/settings', b),
  mekoClaudeSetup: () => post<Settings>('/api/settings/meko/claude-setup'),
  update: () => get<UpdateStatus>('/api/update'),
  checkUpdate: () => post<UpdateStatus>('/api/update/check'),
  applyUpdate: () => post<UpdateStatus>('/api/update/apply'),
};

/** The machine-readable `code` of an API error (DATAPACK_LIMIT, MEKO_OFF, …), if it has one. */
export function errorCode(e: unknown): string | null {
  return e instanceof ApiError && typeof e.body.code === 'string' ? e.body.code : null;
}

export function errorMessage(e: unknown): string {
  if (e instanceof ApiError) return e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}

/** Close code the server uses on a WebSocket whose login is missing or revoked. */
export const WS_UNAUTHORIZED = 4401;

/**
 * After a WebSocket closed with WS_UNAUTHORIZED: if the login really is gone, runs the 401
 * handler (drops to the login screen) and resolves false. Resolves true when still logged in or
 * the server can't be reached, so the caller keeps retrying.
 */
export async function checkLogin(): Promise<boolean> {
  try {
    if ((await api.authStatus()).loggedIn) return true;
  } catch {
    return true;
  }
  onUnauthorized?.();
  return false;
}

export function wsUrl(path: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}${path}`;
}

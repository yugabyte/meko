import type {
  AppState,
  AskAgentkRequest,
  AskAgentkResult,
  AskAnswerRequest,
  AttachDatapackRequest,
  AuthStatus,
  ChangeSide,
  CloseSessionRequest,
  ClaudeCodeStatus,
  CreateProjectRequest,
  CreateSessionRequest,
  CloneRepoRequest,
  DatapackPeople,
  DesignAnswersRequest,
  DesignDoc,
  DiscardCheck,
  InviteCheck,
  LoginRequest,
  MekoCallDetail,
  MekoCallPage,
  MekoDatapack,
  MekoCallBy,
  MekoGap,
  KnowledgeUpload,
  MekoGapMarkRequest,
  MekoGapPage,
  MekoGapState,
  MekoLogInfo,
  MekoTeachRequest,
  MoveSessionRequest,
  OpenPrRequest,
  NewComment,
  PastedImage,
  Project,
  ProjectHome,
  UsageOverview,
  RepoBranches,
  ReposResponse,
  HeldReview,
  HeldReviewsRequest,
  HeldReviewsResult,
  PrReviewers,
  RepoInfo,
  ReviewablePrs,
  TaskPrs,
  LinkPlan,
  StartFromLinkRequest,
  StartFromLinkResult,
  ReviewComment,
  SearchResult,
  SessionMatch,
  SendCommentsResult,
  Session,
  SessionAction,
  SessionChanges,
  Settings,
  SettingsUpdate,
  SetDedicatedDatapackRequest,
  SetupRequest,
  ShareContact,
  ShareDatapackRequest,
  ShareDatapackResult,
  StagedImage,
  TotpEnrollResponse,
  UpdateProjectRequest,
  UpdateSessionRequest,
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

/** `raw` sends those bytes as the body, with their content type, instead of `body` as JSON. */
async function request<T>(
  method: string, path: string, body?: unknown, opts: { quiet401?: boolean; raw?: { data: Blob; type: string }; signal?: AbortSignal } = {},
): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      credentials: 'same-origin',
      headers: opts.raw ? { 'Content-Type': opts.raw.type } : body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: opts.raw ? opts.raw.data : body !== undefined ? JSON.stringify(body) : undefined,
      signal: opts.signal,
    });
  } catch (e) {
    // Cancelled by the caller: theirs to ignore, not a server that's down.
    if (opts.signal?.aborted) throw e;
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
  shareDatapack: (id: string, b: ShareDatapackRequest) => post<ShareDatapackResult>(`/api/meko/datapacks/${enc(id)}/people`, b),
  resendDatapackInvite: (id: string, inviteId: string) =>
    post<DatapackPeople>(`/api/meko/datapacks/${enc(id)}/people/invites/${enc(inviteId)}/resend`),
  revokeDatapackInvite: (id: string, inviteId: string) =>
    request<DatapackPeople>('DELETE', `/api/meko/datapacks/${enc(id)}/people/invites/${enc(inviteId)}`),
  shareContacts: () => get<ShareContact[]>('/api/share-contacts'),
  forgetShareContact: (email: string) => request<void>('DELETE', `/api/share-contacts/${enc(email)}`),
  checkInvites: () => post<InviteCheck>('/api/invites/check'),
  acceptInvite: (datapackId: string) => post<void>(`/api/invites/${enc(datapackId)}/accept`),
  declineInvite: (datapackId: string) => post<void>(`/api/invites/${enc(datapackId)}/decline`),
  laterInvite: (datapackId: string) => post<void>(`/api/invites/${enc(datapackId)}/later`),

  openProject: (b: CreateProjectRequest) => post<Project>('/api/projects', b),
  updateProject: (id: string, b: UpdateProjectRequest) => request<Project>('PATCH', `/api/projects/${enc(id)}`, b),
  closeProject: (id: string) => request<void>('DELETE', `/api/projects/${enc(id)}`),
  project: (id: string) => get<Project>(`/api/projects/${enc(id)}`),
  branches: (id: string) => get<string[]>(`/api/projects/${enc(id)}/branches`),
  attachDatapack: (id: string, b: AttachDatapackRequest) => post<Project>(`/api/projects/${enc(id)}/datapacks`, b),
  setDedicatedDatapack: (id: string, b: SetDedicatedDatapackRequest) => post<Project>(`/api/projects/${enc(id)}/dedicated-datapack`, b),
  detachDatapack: (id: string, dpId: string) =>
    request<Project>('DELETE', `/api/projects/${enc(id)}/datapacks/${enc(dpId)}`),
  home: (id: string) => get<ProjectHome>(`/api/projects/${enc(id)}/home`),
  prReviewers: (id: string) => get<PrReviewers[]>(`/api/projects/${enc(id)}/pr-reviewers`),
  /** The project's PRs to review, or with `pr` the one PR typed (a bare number in `repoPath`). */
  taskPrs: (id: string, q: { pr?: string; repoPath?: string } = {}) => {
    const qs = [q.pr ? `pr=${enc(q.pr)}` : '', q.repoPath ? `repoPath=${enc(q.repoPath)}` : ''].filter(Boolean).join('&');
    return get<TaskPrs>(`/api/projects/${enc(id)}/task-prs${qs ? `?${qs}` : ''}`);
  },
  reviewPrs: (id: string, q: { pr?: string; repoPath?: string } = {}) =>
    get<ReviewablePrs>(`/api/projects/${enc(id)}/review-prs${q.pr ? `?pr=${enc(q.pr)}${q.repoPath ? `&repoPath=${enc(q.repoPath)}` : ''}` : ''}`),
  /** What a pasted GitHub PR or issue (a link or owner/name#N) would start, and where. */
  githubLink: (ref: string, projectId: string | null) => get<LinkPlan>(`/api/github-link?ref=${enc(ref)}${projectId ? `&project=${enc(projectId)}` : ''}`),
  startFromLink: (b: StartFromLinkRequest) => post<StartFromLinkResult>('/api/github-link/start', b),
  askAgentk: (b: AskAgentkRequest, signal: AbortSignal) => request<AskAgentkResult>('POST', '/api/ask-agentk', b, { signal }),
  usage: () => get<UsageOverview>('/api/usage'),
  search: (id: string, query: string) => post<SearchResult[]>(`/api/projects/${enc(id)}/search`, { query }),
  /** Sessions, closed ones too, whose name, task, PR, branch or design has the words in `q`; in `projectId`, or every project open. */
  findSessions: (q: string, projectId: string | null, signal?: AbortSignal) =>
    request<SessionMatch[]>('GET', `/api/sessions/search?q=${enc(q)}${projectId ? `&project=${enc(projectId)}` : ''}`, undefined, { signal }),
  createSession: (projectId: string, b: CreateSessionRequest) =>
    post<Session>(`/api/projects/${enc(projectId)}/sessions`, b),
  /** Saves an image added in the New session dialog, for `CreateSessionRequest.images`; checked by its bytes. */
  stageImage: (projectId: string, image: Blob) =>
    request<StagedImage>('POST', `/api/projects/${enc(projectId)}/images`, undefined, {
      raw: { data: image, type: image.type.startsWith('image/') ? image.type : 'image/png' },
    }),
  dismissFailedStart: (id: string) => request<void>('DELETE', `/api/failed-starts/${enc(id)}`),
  /** merge and discard take a CloseSessionRequest (skip the learn step). */
  sessionAction: (id: string, action: SessionAction, b?: CloseSessionRequest) => post<Session>(`/api/sessions/${enc(id)}/actions/${action}`, b),
  moveSession: (id: string, b: MoveSessionRequest) => post<Session>(`/api/sessions/${enc(id)}/move`, b),
  /** Open PR with the user's instructions (the plain gate uses sessionAction). */
  openPr: (id: string, b: OpenPrRequest) => post<Session>(`/api/sessions/${enc(id)}/actions/open-pr`, b),
  updateSession: (id: string, b: UpdateSessionRequest) => request<Session>('PATCH', `/api/sessions/${enc(id)}`, b),
  changes: (id: string) => get<SessionChanges>(`/api/sessions/${enc(id)}/changes`),
  baseBranches: (id: string) => get<string[]>(`/api/sessions/${enc(id)}/base-branches`),
  /** URL of one side of a changed image; `v` busts the cache when the diff is refetched. */
  changeImageUrl: (id: string, path: string, side: ChangeSide, v: number) =>
    `/api/sessions/${enc(id)}/changes/image?path=${enc(path)}&side=${side}&v=${v}`,
  /** Saves an image pasted into the session's terminal; the server checks its type by its bytes. */
  pasteImage: (id: string, image: Blob) =>
    request<PastedImage>('POST', `/api/sessions/${enc(id)}/images`, undefined, {
      raw: { data: image, type: image.type.startsWith('image/') ? image.type : 'image/png' },
    }),
  /** Adds a file to the knowledge base of the session's project datapack; the server sends it on to Meko. */
  uploadKnowledge: (id: string, file: File) =>
    request<KnowledgeUpload>('POST', `/api/sessions/${enc(id)}/knowledge?name=${enc(file.name)}`, undefined, {
      raw: { data: file, type: 'application/octet-stream' },
    }),
  comments: (id: string) => get<ReviewComment[]>(`/api/sessions/${enc(id)}/comments`),
  addComment: (id: string, b: NewComment) => post<ReviewComment>(`/api/sessions/${enc(id)}/comments`, b),
  editComment: (id: string, cid: string, body: string) =>
    request<ReviewComment>('PATCH', `/api/sessions/${enc(id)}/comments/${enc(cid)}`, { body }),
  deleteComment: (id: string, cid: string) => request<void>('DELETE', `/api/sessions/${enc(id)}/comments/${enc(cid)}`),
  sendComments: (id: string) => post<SendCommentsResult>(`/api/sessions/${enc(id)}/comments/send`),
  heldReviews: (id: string) => get<HeldReview[]>(`/api/sessions/${enc(id)}/held-reviews`),
  decideHeldReviews: (id: string, b: HeldReviewsRequest) => post<HeldReviewsResult>(`/api/sessions/${enc(id)}/held-reviews`, b),
  design: (id: string) => get<DesignDoc>(`/api/sessions/${enc(id)}/design`),
  approveDesign: (id: string, b?: DesignAnswersRequest) => post<Session>(`/api/sessions/${enc(id)}/actions/approve-design`, b),
  reviseDesign: (id: string, b: DesignAnswersRequest) => post<void>(`/api/sessions/${enc(id)}/design/answers`, b),
  answerAsk: (id: string, askId: string, b: AskAnswerRequest) => post<void>(`/api/sessions/${enc(id)}/ask/${enc(askId)}`, b),
  discardCheck: (id: string) => get<DiscardCheck>(`/api/sessions/${enc(id)}/discard-check`),
  mekoLogs: (projectId: string) => get<MekoLogInfo[]>(`/api/projects/${enc(projectId)}/meko-logs`),
  mekoCalls: (id: string, q: { before?: number; tool?: string; by?: MekoCallBy | ''; failed?: boolean } = {}) => {
    const qs = new URLSearchParams();
    if (q.before !== undefined) qs.set('before', String(q.before));
    if (q.tool) qs.set('tool', q.tool);
    if (q.by) qs.set('by', q.by);
    if (q.failed) qs.set('failed', '1');
    const tail = qs.toString();
    return get<MekoCallPage>(`/api/sessions/${enc(id)}/meko-calls${tail ? `?${tail}` : ''}`);
  },
  mekoCall: (id: string, offset: number, callId: string) =>
    get<MekoCallDetail>(`/api/sessions/${enc(id)}/meko-calls/${offset}?id=${enc(callId)}`),
  /** Where the browser downloads the session's Meko call log (a plain link: the login cookie goes with it). */
  mekoLogUrl: (id: string) => `/api/sessions/${enc(id)}/meko-calls.jsonl`,
  clearMekoLog: (id: string) => request<void>('DELETE', `/api/sessions/${enc(id)}/meko-calls`),
  mekoGaps: (projectId: string, q: { state?: MekoGapState | 'all'; after?: { lastAt: number; key: string } } = {}) => {
    const qs = new URLSearchParams();
    if (q.state) qs.set('state', q.state);
    if (q.after) {
      qs.set('before', String(q.after.lastAt));
      qs.set('beforeKey', q.after.key);
    }
    const tail = qs.toString();
    return get<MekoGapPage>(`/api/projects/${enc(projectId)}/meko-gaps${tail ? `?${tail}` : ''}`);
  },
  markMekoGap: (projectId: string, b: MekoGapMarkRequest) => post<void>(`/api/projects/${enc(projectId)}/meko-gaps/mark`, b),
  teachMeko: (projectId: string, b: MekoTeachRequest) => post<MekoGap>(`/api/projects/${enc(projectId)}/meko-gaps/teach`, b),

  settings: () => get<Settings>('/api/settings'),
  saveSettings: (b: SettingsUpdate) => request<Settings>('PUT', '/api/settings', b),
  mekoClaudeSetup: () => post<Settings>('/api/settings/meko/claude-setup'),
  updateClaude: () => post<ClaudeCodeStatus>('/api/claude/update'),
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

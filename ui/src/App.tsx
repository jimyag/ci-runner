import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import {
  AlertCircle,
  CheckCircle2,
  Clock3,
  Copy,
  Loader2,
  Play,
  Plus,
  RefreshCw,
  Square,
  Trash2,
} from "lucide-react"
import { toast } from "sonner"

import { AppSidebar } from "@/components/app-sidebar"
import { SiteHeader } from "@/components/site-header"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Toaster } from "@/components/ui/sonner"
import { cn } from "@/lib/utils"

type RunnerStatus = "queued" | "creating" | "running" | "stopping" | "completed" | "failed"

type RunnerState = {
  id: string
  status: RunnerStatus
  repository_full_name?: string
  profile_name?: string
  runner_group?: string
  runner_name: string
  sandbox_id?: string
  process_pid?: number
  assigned_job_id?: number
  assigned_job_name?: string
  error?: string
  failure_stage?: string
  failure_reason?: string
  updated_at: string
  created_at: string
  completed_at?: string
}

type RunnerProfile = {
  name: string
  labels: string[]
  template_id: string
  runner_group?: string
  max_concurrency: number
  min_idle: number
  priority: number
  enabled: boolean
  created_at: string
  updated_at: string
}

type RepositoryPolicy = {
  id: number
  repository_full_name: string
  profile_name: string
  enabled: boolean
  created_at: string
}

type ProfileMatch = {
  repository_full_name: string
  labels: string[]
  profile?: RunnerProfile
  reason?: string
}

type DiagnosticsSummary = {
  pprof: Array<{ address: string; address_file: string; dump_script: string }>
  state: { backend: string; database: string }
  github: { auth_mode: string; installation_id?: number; api_base_url: string }
  sandbox: { api_url: string; domain: string }
  recent_failures: RunnerState[]
}

type Metric = {
  label: string
  value: number
  description: string
}

const tokenKey = "runnerd-admin-token"
const activeStatuses = new Set<RunnerStatus>(["queued", "creating", "running", "stopping"])
const logNames = ["control.log", "stdout.log", "stderr.log"] as const

function App() {
  const [token, setTokenState] = useState(() => localStorage.getItem(tokenKey) || "")
  const [tokenInput, setTokenInput] = useState("")
  const [section, setSection] = useState("overview")
  const [runners, setRunners] = useState<RunnerState[]>([])
  const [profiles, setProfiles] = useState<RunnerProfile[]>([])
  const [policies, setPolicies] = useState<RepositoryPolicy[]>([])
  const [selectedID, setSelectedID] = useState("")
  const [selectedLog, setSelectedLog] = useState<(typeof logNames)[number]>("control.log")
  const [logText, setLogText] = useState("No runner selected")
  const [loading, setLoading] = useState(false)
  const [connected, setConnected] = useState(false)
  const [lastUpdated, setLastUpdated] = useState("")
  const [createID, setCreateID] = useState("")
  const [createRepository, setCreateRepository] = useState("")
  const [createProfile, setCreateProfile] = useState("")
  const [createLabels, setCreateLabels] = useState("self-hosted,e2b")
  const [profileForm, setProfileForm] = useState({
    name: "",
    labels: "self-hosted,e2b",
    template_id: "",
    runner_group: "default",
    max_concurrency: "10",
    min_idle: "0",
    priority: "0",
    enabled: true,
  })
  const [policyForm, setPolicyForm] = useState({
    id: 0,
    repository_full_name: "",
    profile_name: "",
    enabled: true,
  })
  const [matchRepository, setMatchRepository] = useState("")
  const [matchLabels, setMatchLabels] = useState("self-hosted,e2b")
  const [matchResult, setMatchResult] = useState<ProfileMatch | null>(null)
  const [diagnostics, setDiagnostics] = useState<DiagnosticsSummary | null>(null)
  const [diagnosticsVars, setDiagnosticsVars] = useState("")
  const createIDRef = useRef<HTMLInputElement>(null)

  const selected = useMemo(
    () => runners.find((runner) => runner.id === selectedID),
    [runners, selectedID]
  )

  const metrics = useMemo<Metric[]>(() => {
    const count = (status: RunnerStatus) => runners.filter((runner) => runner.status === status).length
    return [
      {
        label: "Active",
        value: runners.filter((runner) => activeStatuses.has(runner.status)).length,
        description: "queued / creating / running / stopping",
      },
      { label: "Completed", value: count("completed"), description: "cleaned after exit" },
      { label: "Failed", value: count("failed"), description: "needs inspection" },
      { label: "Profiles", value: profiles.length, description: "active control-plane profiles" },
    ]
  }, [profiles.length, runners])

  const setToken = useCallback((value: string) => {
    const next = value.trim()
    setTokenState(next)
    if (next) localStorage.setItem(tokenKey, next)
    else localStorage.removeItem(tokenKey)
  }, [])

  const request = useCallback(
    async (url: string, options: RequestInit = {}) => {
      const headers = new Headers(options.headers)
      headers.set("Authorization", `Bearer ${token}`)
      const response = await fetch(url, { ...options, headers })
      if (response.status === 401) {
        setToken("")
        setConnected(false)
        throw new Error("Invalid ADMIN_TOKEN")
      }
      if (!response.ok) {
        const text = await response.text()
        throw new Error(text || `${response.status} ${response.statusText}`)
      }
      const contentType = response.headers.get("content-type") || ""
      if (contentType.includes("application/json")) return response.json()
      return response.text()
    },
    [setToken, token]
  )

  const parseLabels = (value: string) =>
    value
      .split(",")
      .map((label) => label.trim())
      .filter(Boolean)

  const loadLog = useCallback(
    async (id: string, name: (typeof logNames)[number]) => {
      if (!token || !id) {
        setLogText("No runner selected")
        return
      }
      setLogText("Loading...")
      try {
        const text = (await request(
          `/runners/${encodeURIComponent(id)}/logs/${encodeURIComponent(name)}`
        )) as string
        setLogText(text || "Log is empty")
      } catch (error) {
        setLogText(error instanceof Error ? error.message : "Failed to load log")
      }
    },
    [request, token]
  )

  const loadAll = useCallback(async () => {
    if (!token) {
      setConnected(false)
      return
    }
    setLoading(true)
    try {
      const [runnerData, profileData, policyData] = await Promise.all([
        request("/runners"),
        request("/profiles"),
        request("/repository-policies"),
      ])
      const nextRunners = Array.isArray(runnerData) ? (runnerData as RunnerState[]) : []
      setRunners(nextRunners)
      setProfiles(Array.isArray(profileData) ? (profileData as RunnerProfile[]) : [])
      setPolicies(Array.isArray(policyData) ? (policyData as RepositoryPolicy[]) : [])
      setConnected(true)
      setLastUpdated(new Date().toLocaleTimeString())
      if (selectedID && !nextRunners.some((runner) => runner.id === selectedID)) {
        setSelectedID("")
        setLogText("No runner selected")
      }
    } catch (error) {
      setConnected(false)
      toast.error(error instanceof Error ? error.message : "Failed to load control plane data")
    } finally {
      setLoading(false)
    }
  }, [request, selectedID, token])

  useEffect(() => {
    void fetch("/healthz").catch(() => setConnected(false))
  }, [])

  useEffect(() => {
    void loadAll()
    const timer = window.setInterval(() => void loadAll(), 5000)
    return () => window.clearInterval(timer)
  }, [loadAll])

  useEffect(() => {
    if (selectedID) void loadLog(selectedID, selectedLog)
  }, [loadLog, selectedID, selectedLog])

  useEffect(() => {
    if (section !== "diagnostics" || !token) return
    void (async () => {
      try {
        const [summary, vars] = await Promise.all([
          request("/diagnostics/pprof"),
          request("/diagnostics/vars").catch(() => ""),
        ])
        setDiagnostics(summary as DiagnosticsSummary)
        setDiagnosticsVars(typeof vars === "string" ? vars : JSON.stringify(vars, null, 2))
      } catch (error) {
        toast.error(error instanceof Error ? error.message : "Failed to load diagnostics")
      }
    })()
  }, [request, section, token])

  const submitToken = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setToken(tokenInput)
    setTokenInput("")
    toast.success("Admin token saved")
  }

  const clearToken = () => {
    setToken("")
    setRunners([])
    setProfiles([])
    setPolicies([])
    setSelectedID("")
    setLogText("No runner selected")
  }

  const createRunner = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (!token) {
      toast.error("ADMIN_TOKEN required")
      return
    }
    const body: {
      id?: string
      repository_full_name?: string
      profile_name?: string
      labels?: string[]
    } = {}
    if (createID.trim()) body.id = createID.trim()
    if (createRepository.trim()) body.repository_full_name = createRepository.trim()
    if (createProfile.trim()) body.profile_name = createProfile.trim()
    const labels = parseLabels(createLabels)
    if (labels.length > 0) body.labels = labels
    try {
      const runner = (await request("/runners", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })) as RunnerState
      setCreateID("")
      setSelectedID(runner.id)
      toast.success(`Runner ${runner.id} queued`)
      await loadAll()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to create runner")
    }
  }

  const stopRunner = async (id: string) => {
    try {
      const runner = (await request(`/runners/${encodeURIComponent(id)}`, {
        method: "DELETE",
      })) as RunnerState
      setSelectedID(runner.id)
      toast.success(`Runner ${runner.id} completed`)
      await loadAll()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to stop runner")
    }
  }

  const saveProfile = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    try {
      const payload = {
        name: profileForm.name.trim(),
        labels: parseLabels(profileForm.labels),
        template_id: profileForm.template_id.trim(),
        runner_group: profileForm.runner_group.trim(),
        max_concurrency: Number(profileForm.max_concurrency) || 0,
        min_idle: Number(profileForm.min_idle) || 0,
        priority: Number(profileForm.priority) || 0,
        enabled: profileForm.enabled,
      }
      const isUpdate = profiles.some((profile) => profile.name === payload.name)
      const url = isUpdate ? `/profiles/${encodeURIComponent(payload.name)}` : "/profiles"
      const method = isUpdate ? "PATCH" : "POST"
      await request(url, {
        method,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      })
      toast.success(`Profile ${payload.name} saved`)
      await loadAll()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to save profile")
    }
  }

  const loadProfileIntoForm = (profile: RunnerProfile) => {
    setSection("profiles")
    setProfileForm({
      name: profile.name,
      labels: profile.labels.join(","),
      template_id: profile.template_id,
      runner_group: profile.runner_group || "",
      max_concurrency: String(profile.max_concurrency),
      min_idle: String(profile.min_idle),
      priority: String(profile.priority),
      enabled: profile.enabled,
    })
  }

  const deleteProfile = async (name: string) => {
    try {
      await request(`/profiles/${encodeURIComponent(name)}`, { method: "DELETE" })
      toast.success(`Profile ${name} deleted`)
      if (profileForm.name === name) {
        setProfileForm({
          name: "",
          labels: "self-hosted,e2b",
          template_id: "",
          runner_group: "default",
          max_concurrency: "10",
          min_idle: "0",
          priority: "0",
          enabled: true,
        })
      }
      await loadAll()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to delete profile")
    }
  }

  const savePolicy = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    try {
      const payload = {
        repository_full_name: policyForm.repository_full_name.trim(),
        profile_name: policyForm.profile_name.trim(),
        enabled: policyForm.enabled,
      }
      const isUpdate = policyForm.id > 0
      const url = isUpdate ? `/repository-policies/${policyForm.id}` : "/repository-policies"
      const method = isUpdate ? "PATCH" : "POST"
      await request(url, {
        method,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      })
      toast.success("Repository policy saved")
      await loadAll()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to save repository policy")
    }
  }

  const loadPolicyIntoForm = (policy: RepositoryPolicy) => {
    setSection("policies")
    setPolicyForm({
      id: policy.id,
      repository_full_name: policy.repository_full_name,
      profile_name: policy.profile_name,
      enabled: policy.enabled,
    })
  }

  const deletePolicy = async (id: number) => {
    try {
      await request(`/repository-policies/${id}`, { method: "DELETE" })
      toast.success("Repository policy deleted")
      if (policyForm.id === id) {
        setPolicyForm({ id: 0, repository_full_name: "", profile_name: "", enabled: true })
      }
      await loadAll()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to delete repository policy")
    }
  }

  const runMatchTest = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    try {
      const result = (await request("/profiles/match-test", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          repository_full_name: matchRepository.trim(),
          labels: parseLabels(matchLabels),
        }),
      })) as ProfileMatch
      setMatchResult(result)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to run match test")
    }
  }

  const copySelectedID = async () => {
    if (!selected) return
    await navigator.clipboard.writeText(selected.id)
    toast.success("Runner ID copied")
  }

  return (
    <SidebarProvider>
      <AppSidebar
        connected={connected}
        activeCount={metrics[0]?.value || 0}
        onRefresh={() => void loadAll()}
        onCreateFocus={() => createIDRef.current?.focus()}
        onClearToken={clearToken}
      />
      <SidebarInset>
        <SiteHeader />
        <main className="flex flex-1 flex-col gap-4 p-4 lg:gap-6 lg:p-6">
          {!token ? (
            <Card className="max-w-xl">
              <CardHeader>
                <CardTitle>Admin access</CardTitle>
                <CardDescription>Use the runnerd ADMIN_TOKEN.</CardDescription>
              </CardHeader>
              <CardContent>
                <form className="flex flex-col gap-3 sm:flex-row" onSubmit={submitToken}>
                  <Input
                    type="password"
                    value={tokenInput}
                    onChange={(event) => setTokenInput(event.target.value)}
                    placeholder="ADMIN_TOKEN"
                    autoComplete="current-password"
                  />
                  <Button type="submit">Connect</Button>
                </form>
              </CardContent>
            </Card>
          ) : null}

          <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
            {metrics.map((metric) => (
              <Card key={metric.label} className="gap-3 py-5">
                <CardHeader className="px-5">
                  <CardDescription>{metric.label}</CardDescription>
                  <CardTitle className="text-3xl">{metric.value}</CardTitle>
                </CardHeader>
                <CardContent className="px-5 text-xs text-muted-foreground">
                  {metric.description}
                </CardContent>
              </Card>
            ))}
          </div>

          <Card className="py-4">
            <CardContent className="flex flex-col gap-3 px-5 lg:flex-row lg:items-center lg:justify-between">
              <div>
                <div className="text-sm font-medium">Control plane</div>
                <div className="text-xs text-muted-foreground">
                  {lastUpdated ? `Updated ${lastUpdated}` : "Waiting for control plane data"}
                </div>
              </div>
              <Tabs value={section} onValueChange={setSection}>
                <TabsList>
                  <TabsTrigger value="overview">Overview</TabsTrigger>
                  <TabsTrigger value="runners">Runner Requests</TabsTrigger>
                  <TabsTrigger value="profiles">Profiles</TabsTrigger>
                  <TabsTrigger value="policies">Repository Policies</TabsTrigger>
                  <TabsTrigger value="match">Match Test</TabsTrigger>
                  <TabsTrigger value="diagnostics">Diagnostics</TabsTrigger>
                </TabsList>
              </Tabs>
            </CardContent>
          </Card>

          {section === "overview" ? (
            <div className="grid gap-4 xl:grid-cols-2">
              <Card>
                <CardHeader>
                  <CardTitle>Recent runner requests</CardTitle>
                  <CardDescription>Newest requests and their matched profiles.</CardDescription>
                </CardHeader>
                <CardContent className="space-y-3">
                  {runners.slice(0, 8).map((runner) => (
                    <div key={runner.id} className="flex items-center justify-between gap-3 rounded-md border p-3">
                      <div className="min-w-0">
                        <div className="truncate font-medium">{runner.repository_full_name || runner.id}</div>
                        <div className="truncate text-xs text-muted-foreground">
                          {runner.profile_name || "-"} · {runner.runner_name}
                        </div>
                      </div>
                      <StatusBadge status={runner.status} />
                    </div>
                  ))}
                  {runners.length === 0 ? (
                    <div className="text-sm text-muted-foreground">No runner requests yet.</div>
                  ) : null}
                </CardContent>
              </Card>
              <Card>
                <CardHeader>
                  <CardTitle>Profiles and repository policies</CardTitle>
                  <CardDescription>Current seeded and runtime-managed routing rules.</CardDescription>
                </CardHeader>
                <CardContent className="grid gap-4 lg:grid-cols-2">
                  <div className="space-y-3">
                    <div className="text-sm font-medium">Profiles</div>
                    {profiles.map((profile) => (
                      <div
                        key={profile.name}
                        className="rounded-md border p-3 text-sm"
                        onClick={() => loadProfileIntoForm(profile)}
                      >
                        <div className="font-medium">{profile.name}</div>
                        <div className="text-xs text-muted-foreground">
                          {profile.labels.join(", ")} · template {profile.template_id}
                        </div>
                      </div>
                    ))}
                  </div>
                  <div className="space-y-3">
                    <div className="text-sm font-medium">Repository policies</div>
                    {policies.map((policy) => (
                      <div
                        key={policy.id}
                        className="rounded-md border p-3 text-sm"
                        onClick={() => loadPolicyIntoForm(policy)}
                      >
                        <div className="font-medium">{policy.repository_full_name}</div>
                        <div className="text-xs text-muted-foreground">{policy.profile_name}</div>
                      </div>
                    ))}
                  </div>
                </CardContent>
              </Card>
            </div>
          ) : null}

          {section === "runners" ? (
            <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_420px]">
              <Card className="min-w-0 gap-0 py-0">
                <CardHeader className="border-b px-5 py-4">
                  <div className="flex flex-col gap-3">
                    <div>
                      <CardTitle>Runner Requests</CardTitle>
                      <CardDescription>
                        Webhook and manual requests with matched repository/policy context.
                      </CardDescription>
                    </div>
                    <form className="grid gap-2 lg:grid-cols-4" onSubmit={createRunner}>
                      <Input
                        ref={createIDRef}
                        value={createID}
                        onChange={(event) => setCreateID(event.target.value)}
                        placeholder="optional id"
                      />
                      <Input
                        value={createRepository}
                        onChange={(event) => setCreateRepository(event.target.value)}
                        placeholder="owner/repo or owner/*"
                      />
                      <Input
                        value={createProfile}
                        onChange={(event) => setCreateProfile(event.target.value)}
                        placeholder="optional profile"
                      />
                      <Input
                        value={createLabels}
                        onChange={(event) => setCreateLabels(event.target.value)}
                        placeholder="self-hosted,e2b"
                      />
                      <div className="flex gap-2 lg:col-span-4">
                        <Button type="submit" disabled={!token}>
                          <Plus />
                          Create
                        </Button>
                        <Button
                          type="button"
                          variant="outline"
                          size="icon"
                          onClick={() => void loadAll()}
                          disabled={loading}
                          title="Refresh"
                        >
                          <RefreshCw className={cn(loading && "animate-spin")} />
                        </Button>
                      </div>
                    </form>
                  </div>
                </CardHeader>
                <CardContent className="p-0">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Status</TableHead>
                        <TableHead>Repository</TableHead>
                        <TableHead>Profile</TableHead>
                        <TableHead>Runner</TableHead>
                        <TableHead>Sandbox</TableHead>
                        <TableHead>Updated</TableHead>
                        <TableHead className="w-24" />
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {runners.length === 0 ? (
                        <TableRow>
                          <TableCell colSpan={7} className="h-24 text-center text-muted-foreground">
                            No runner requests found
                          </TableCell>
                        </TableRow>
                      ) : (
                        runners.map((runner) => (
                          <TableRow
                            key={runner.id}
                            data-state={runner.id === selectedID ? "selected" : undefined}
                            className="cursor-pointer"
                            onClick={() => setSelectedID(runner.id)}
                          >
                            <TableCell>
                              <StatusBadge status={runner.status} />
                            </TableCell>
                            <TableCell className="max-w-[220px] truncate">
                              {runner.repository_full_name || "-"}
                            </TableCell>
                            <TableCell>{runner.profile_name || "-"}</TableCell>
                            <TableCell>
                              <div className="font-medium">{runner.runner_name || runner.id}</div>
                              <div className="text-xs text-muted-foreground">{runner.id}</div>
                            </TableCell>
                            <TableCell className="max-w-[180px] truncate">
                              {runner.sandbox_id || "-"}
                            </TableCell>
                            <TableCell>{formatTime(runner.updated_at)}</TableCell>
                            <TableCell>
                              <Button
                                type="button"
                                variant="outline"
                                size="sm"
                                onClick={(event) => {
                                  event.stopPropagation()
                                  void stopRunner(runner.id)
                                }}
                              >
                                <Trash2 />
                                Stop
                              </Button>
                            </TableCell>
                          </TableRow>
                        ))
                      )}
                    </TableBody>
                  </Table>
                </CardContent>
              </Card>

              <Card className="min-w-0 gap-0 py-0">
                <CardHeader className="border-b px-5 py-4">
                  <div className="flex items-start justify-between gap-3">
                    <div>
                      <CardTitle>Request details</CardTitle>
                      <CardDescription>{selected?.runner_name || "Select a request"}</CardDescription>
                    </div>
                    <Button
                      type="button"
                      variant="outline"
                      size="icon"
                      onClick={() => void copySelectedID()}
                      disabled={!selected}
                      title="Copy runner ID"
                    >
                      <Copy />
                    </Button>
                  </div>
                </CardHeader>
                {selected ? (
                  <CardContent className="grid gap-4 p-5">
                    <div className="grid grid-cols-[110px_minmax(0,1fr)] gap-x-3 gap-y-2 text-sm">
                      <Detail label="ID" value={selected.id} />
                      <Detail label="Status" value={selected.status} />
                      <Detail label="Repository" value={selected.repository_full_name || "-"} />
                      <Detail label="Profile" value={selected.profile_name || "-"} />
                      <Detail label="Sandbox" value={selected.sandbox_id || "-"} />
                      <Detail label="PID" value={selected.process_pid || "-"} />
                      <Detail
                        label="Job"
                        value={selected.assigned_job_name || selected.assigned_job_id || "-"}
                      />
                      <Detail label="Created" value={formatTime(selected.created_at)} />
                      <Detail label="Updated" value={formatTime(selected.updated_at)} />
                      <Detail label="Completed" value={formatTime(selected.completed_at)} />
                      <Detail label="Failure" value={selected.failure_reason || "-"} />
                      <Detail label="Error" value={selected.error || "-"} />
                    </div>
                    <Tabs
                      value={selectedLog}
                      onValueChange={(value) => setSelectedLog(value as (typeof logNames)[number])}
                    >
                      <TabsList>
                        {logNames.map((name) => (
                          <TabsTrigger key={name} value={name}>
                            {name.replace(".log", "")}
                          </TabsTrigger>
                        ))}
                      </TabsList>
                    </Tabs>
                    <pre className="max-h-[48vh] min-h-72 overflow-auto rounded-lg border bg-muted/50 p-3 text-xs leading-relaxed whitespace-pre-wrap">
                      {logText}
                    </pre>
                  </CardContent>
                ) : (
                  <CardContent className="p-8 text-sm text-muted-foreground">
                    No runner request selected
                  </CardContent>
                )}
              </Card>
            </div>
          ) : null}

          {section === "profiles" ? (
            <div className="grid gap-4 xl:grid-cols-[420px_minmax(0,1fr)]">
              <Card>
                <CardHeader>
                  <CardTitle>Profile editor</CardTitle>
                  <CardDescription>Create or update runner profiles.</CardDescription>
                </CardHeader>
                <CardContent>
                  <form className="grid gap-3" onSubmit={saveProfile}>
                    <Input
                      value={profileForm.name}
                      onChange={(event) => setProfileForm((current) => ({ ...current, name: event.target.value }))}
                      placeholder="profile name"
                    />
                    <Input
                      value={profileForm.labels}
                      onChange={(event) => setProfileForm((current) => ({ ...current, labels: event.target.value }))}
                      placeholder="self-hosted,e2b"
                    />
                    <Input
                      value={profileForm.template_id}
                      onChange={(event) => setProfileForm((current) => ({ ...current, template_id: event.target.value }))}
                      placeholder="template id"
                    />
                    <Input
                      value={profileForm.runner_group}
                      onChange={(event) => setProfileForm((current) => ({ ...current, runner_group: event.target.value }))}
                      placeholder="runner group"
                    />
                    <div className="grid grid-cols-3 gap-2">
                      <Input
                        value={profileForm.max_concurrency}
                        onChange={(event) => setProfileForm((current) => ({ ...current, max_concurrency: event.target.value }))}
                        placeholder="max concurrency"
                      />
                      <Input
                        value={profileForm.min_idle}
                        onChange={(event) => setProfileForm((current) => ({ ...current, min_idle: event.target.value }))}
                        placeholder="min idle"
                      />
                      <Input
                        value={profileForm.priority}
                        onChange={(event) => setProfileForm((current) => ({ ...current, priority: event.target.value }))}
                        placeholder="priority"
                      />
                    </div>
                    <label className="flex items-center gap-2 text-sm">
                      <input
                        type="checkbox"
                        checked={profileForm.enabled}
                        onChange={(event) => setProfileForm((current) => ({ ...current, enabled: event.target.checked }))}
                      />
                      enabled
                    </label>
                    <div className="flex gap-2">
                      <Button type="submit">Save profile</Button>
                      <Button
                        type="button"
                        variant="outline"
                        onClick={() =>
                          setProfileForm({
                            name: "",
                            labels: "self-hosted,e2b",
                            template_id: "",
                            runner_group: "default",
                            max_concurrency: "10",
                            min_idle: "0",
                            priority: "0",
                            enabled: true,
                          })
                        }
                      >
                        Reset
                      </Button>
                    </div>
                  </form>
                </CardContent>
              </Card>
              <Card className="min-w-0">
                <CardHeader>
                  <CardTitle>Profiles</CardTitle>
                  <CardDescription>Click a profile to edit it.</CardDescription>
                </CardHeader>
                <CardContent className="p-0">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Name</TableHead>
                        <TableHead>Labels</TableHead>
                        <TableHead>Template</TableHead>
                        <TableHead>Runner group</TableHead>
                        <TableHead>Limit</TableHead>
                        <TableHead className="w-24" />
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {profiles.map((profile) => (
                        <TableRow key={profile.name} className="cursor-pointer" onClick={() => loadProfileIntoForm(profile)}>
                          <TableCell>{profile.name}</TableCell>
                          <TableCell className="max-w-[260px] truncate">{profile.labels.join(", ")}</TableCell>
                          <TableCell>{profile.template_id}</TableCell>
                          <TableCell>{profile.runner_group || "-"}</TableCell>
                          <TableCell>{profile.max_concurrency}</TableCell>
                          <TableCell>
                            <Button
                              type="button"
                              variant="outline"
                              size="sm"
                              onClick={(event) => {
                                event.stopPropagation()
                                void deleteProfile(profile.name)
                              }}
                            >
                              <Trash2 />
                              Delete
                            </Button>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </CardContent>
              </Card>
            </div>
          ) : null}

          {section === "policies" ? (
            <div className="grid gap-4 xl:grid-cols-[420px_minmax(0,1fr)]">
              <Card>
                <CardHeader>
                  <CardTitle>Repository policy editor</CardTitle>
                  <CardDescription>Bind repositories to allowed profiles.</CardDescription>
                </CardHeader>
                <CardContent>
                  <form className="grid gap-3" onSubmit={savePolicy}>
                    <Input
                      value={policyForm.repository_full_name}
                      onChange={(event) =>
                        setPolicyForm((current) => ({ ...current, repository_full_name: event.target.value }))
                      }
                      placeholder="owner/repo or owner/*"
                    />
                    <Input
                      value={policyForm.profile_name}
                      onChange={(event) =>
                        setPolicyForm((current) => ({ ...current, profile_name: event.target.value }))
                      }
                      placeholder="profile name"
                    />
                    <label className="flex items-center gap-2 text-sm">
                      <input
                        type="checkbox"
                        checked={policyForm.enabled}
                        onChange={(event) => setPolicyForm((current) => ({ ...current, enabled: event.target.checked }))}
                      />
                      enabled
                    </label>
                    <div className="flex gap-2">
                      <Button type="submit">Save policy</Button>
                      <Button
                        type="button"
                        variant="outline"
                        onClick={() =>
                          setPolicyForm({ id: 0, repository_full_name: "", profile_name: "", enabled: true })
                        }
                      >
                        Reset
                      </Button>
                    </div>
                  </form>
                </CardContent>
              </Card>
              <Card className="min-w-0">
                <CardHeader>
                  <CardTitle>Repository policies</CardTitle>
                  <CardDescription>Click a policy row to edit it.</CardDescription>
                </CardHeader>
                <CardContent className="p-0">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Repository</TableHead>
                        <TableHead>Profile</TableHead>
                        <TableHead>Enabled</TableHead>
                        <TableHead>Created</TableHead>
                        <TableHead className="w-24" />
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {policies.map((policy) => (
                        <TableRow key={policy.id} className="cursor-pointer" onClick={() => loadPolicyIntoForm(policy)}>
                          <TableCell>{policy.repository_full_name}</TableCell>
                          <TableCell>{policy.profile_name}</TableCell>
                          <TableCell>{policy.enabled ? "yes" : "no"}</TableCell>
                          <TableCell>{formatTime(policy.created_at)}</TableCell>
                          <TableCell>
                            <Button
                              type="button"
                              variant="outline"
                              size="sm"
                              onClick={(event) => {
                                event.stopPropagation()
                                void deletePolicy(policy.id)
                              }}
                            >
                              <Trash2 />
                              Delete
                            </Button>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </CardContent>
              </Card>
            </div>
          ) : null}

          {section === "match" ? (
            <div className="grid gap-4 xl:grid-cols-[420px_minmax(0,1fr)]">
              <Card>
                <CardHeader>
                  <CardTitle>Label matching test</CardTitle>
                  <CardDescription>Preview which profile a repository and label set would use.</CardDescription>
                </CardHeader>
                <CardContent>
                  <form className="grid gap-3" onSubmit={runMatchTest}>
                    <Input
                      value={matchRepository}
                      onChange={(event) => setMatchRepository(event.target.value)}
                      placeholder="owner/repo"
                    />
                    <Input
                      value={matchLabels}
                      onChange={(event) => setMatchLabels(event.target.value)}
                      placeholder="self-hosted,e2b"
                    />
                    <Button type="submit">Run match</Button>
                  </form>
                </CardContent>
              </Card>
              <Card>
                <CardHeader>
                  <CardTitle>Match result</CardTitle>
                  <CardDescription>Repository policy + label coverage resolution.</CardDescription>
                </CardHeader>
                <CardContent className="space-y-3">
                  {matchResult ? (
                    <>
                      <Detail label="Repository" value={matchResult.repository_full_name || "-"} />
                      <Detail label="Labels" value={matchResult.labels.join(", ") || "-"} />
                      <Detail label="Profile" value={matchResult.profile?.name || "-"} />
                      <Detail label="Reason" value={matchResult.reason || "matched"} />
                    </>
                  ) : (
                    <div className="text-sm text-muted-foreground">No match run yet.</div>
                  )}
                </CardContent>
              </Card>
            </div>
          ) : null}

          {section === "diagnostics" ? (
            <div className="grid gap-4 xl:grid-cols-2">
              <Card>
                <CardHeader>
                  <CardTitle>Diagnostics summary</CardTitle>
                  <CardDescription>DB, GitHub auth, sandbox API, and pprof discovery.</CardDescription>
                </CardHeader>
                <CardContent className="space-y-3">
                  <Detail label="State backend" value={diagnostics?.state.backend || "-"} />
                  <Detail label="Database" value={diagnostics?.state.database || "-"} />
                  <Detail label="GitHub auth" value={diagnostics?.github.auth_mode || "-"} />
                  <Detail label="Installation" value={diagnostics?.github.installation_id || "-"} />
                  <Detail label="GitHub API" value={diagnostics?.github.api_base_url || "-"} />
                  <Detail label="Sandbox API" value={diagnostics?.sandbox.api_url || "-"} />
                  <Detail label="Sandbox domain" value={diagnostics?.sandbox.domain || "-"} />
                  <div className="space-y-2">
                    <div className="text-sm font-medium">pprof endpoints</div>
                    {diagnostics?.pprof?.length ? (
                      diagnostics.pprof.map((item) => (
                        <div key={item.address_file} className="rounded-md border p-3 text-xs">
                          <div className="font-medium">{item.address}</div>
                          <div className="text-muted-foreground">{item.address_file}</div>
                          <div className="text-muted-foreground">{item.dump_script}</div>
                        </div>
                      ))
                    ) : (
                      <div className="text-sm text-muted-foreground">No pprof artifact discovered yet.</div>
                    )}
                  </div>
                </CardContent>
              </Card>
              <Card>
                <CardHeader>
                  <CardTitle>Recent failures</CardTitle>
                  <CardDescription>Latest failed requests plus the current /debug/vars snapshot.</CardDescription>
                </CardHeader>
                <CardContent className="space-y-4">
                  <div className="space-y-2">
                    {diagnostics?.recent_failures?.length ? (
                      diagnostics.recent_failures.map((failure) => (
                        <div key={failure.id} className="rounded-md border p-3 text-sm">
                          <div className="font-medium">{failure.id}</div>
                          <div className="text-xs text-muted-foreground">
                            {failure.repository_full_name || "-"} · {failure.profile_name || "-"} ·{" "}
                            {failure.failure_reason || failure.error || "-"}
                          </div>
                        </div>
                      ))
                    ) : (
                      <div className="text-sm text-muted-foreground">No recent failures.</div>
                    )}
                  </div>
                  <pre className="max-h-[48vh] min-h-72 overflow-auto rounded-lg border bg-muted/50 p-3 text-xs leading-relaxed whitespace-pre-wrap">
                    {diagnosticsVars || "No /debug/vars data available"}
                  </pre>
                </CardContent>
              </Card>
            </div>
          ) : null}
        </main>
      </SidebarInset>
      <Toaster richColors />
    </SidebarProvider>
  )
}

function StatusBadge({ status }: { status: RunnerStatus }) {
  if (status === "running") {
    return (
      <Badge variant="success">
        <Play />
        running
      </Badge>
    )
  }
  if (status === "failed") {
    return (
      <Badge variant="danger">
        <AlertCircle />
        failed
      </Badge>
    )
  }
  if (status === "completed") {
    return (
      <Badge variant="outline">
        <Square />
        completed
      </Badge>
    )
  }
  if (status === "creating") {
    return (
      <Badge variant="warning">
        <Loader2 className="animate-spin" />
        creating
      </Badge>
    )
  }
  if (status === "stopping") {
    return (
      <Badge variant="warning">
        <Clock3 />
        stopping
      </Badge>
    )
  }
  return (
    <Badge variant="secondary">
      <CheckCircle2 />
      queued
    </Badge>
  )
}

function Detail({ label, value }: { label: string; value: string | number }) {
  return (
    <div className="grid grid-cols-[110px_minmax(0,1fr)] gap-x-3 gap-y-2 text-sm">
      <div className="text-muted-foreground">{label}</div>
      <div className="min-w-0 break-words font-medium">{value}</div>
    </div>
  )
}

function formatTime(value?: string) {
  if (!value) return "-"
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}

export default App

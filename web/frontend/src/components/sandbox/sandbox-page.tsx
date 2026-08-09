"use client"

import { useState, useMemo } from "react"
import { useTranslation } from "react-i18next"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { IconCheck, IconLoader, IconX } from "@tabler/icons-react"

import { PageHeader } from "@/components/page-header"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import {
  putFixtures,
  reloadFixtures,
  resetSandboxState,
  type FixtureEntry,
} from "@/api/sandbox"
import { useSandboxStatus, useFixtures } from "@/hooks/use-sandbox-status"

export function SandboxPage() {
  const { t } = useTranslation()
  const { data: status, isLoading, error } = useSandboxStatus()
  const [selectedVenue, setSelectedVenue] = useState<string | null>(null)
  const [editingBody, setEditingBody] = useState<Record<string, string>>({})
  const [editingQuery, setEditingQuery] = useState<Record<string, string>>({})
  const [jsonErrors, setJsonErrors] = useState<Record<string, string>>({})
  const [saveMessages, setSaveMessages] = useState<Record<string, { type: string; text: string }>>({})
  const [resetMessage, setResetMessage] = useState<{ type: string; text: string } | null>(null)
  const { data: fixtures, isLoading: fixturesLoading, error: fixturesError } = useFixtures(selectedVenue)
  const queryClient = useQueryClient()

  // Track which fixtures are being edited
  const editedFixtureIndices = useMemo(() => {
    if (!fixtures) return new Set<number>()
    return new Set(
      fixtures
        .map((_, i) => (editingBody[i] !== undefined || editingQuery[i] !== undefined ? i : null))
        .filter((i) => i !== null),
    )
  }, [fixtures, editingBody, editingQuery])

  // Render a fixture body for the editor. `body` comes from Go's json.RawMessage,
  // so it is already parsed (object/array/scalar); only pre-existing string bodies
  // need a parse attempt.
  const formatBody = (response: FixtureEntry["response"]) => {
    const { body } = response
    if (body === undefined || body === null) {
      return response.body_text || ""
    }
    if (typeof body === "string") {
      try {
        return JSON.stringify(JSON.parse(body), null, 2)
      } catch {
        return body
      }
    }
    return JSON.stringify(body, null, 2)
  }

  // Validate and parse JSON body
  const validateJSON = (index: number, value: string) => {
    const key = String(index)
    if (!value.trim()) {
      setJsonErrors((prev) => {
        const updated = { ...prev }
        delete updated[key]
        return updated
      })
      return true
    }
    try {
      JSON.parse(value)
      setJsonErrors((prev) => {
        const updated = { ...prev }
        delete updated[key]
        return updated
      })
      return true
    } catch (e) {
      setJsonErrors((prev) => ({
        ...prev,
        [key]: (e as Error).message,
      }))
      return false
    }
  }

  // Handle body change
  const handleBodyChange = (index: number, value: string) => {
    setEditingBody((prev) => ({
      ...prev,
      [index]: value,
    }))
    validateJSON(index, value)
  }

  // Prepare fixtures for saving
  const getFixturesToSave = () => {
    if (!fixtures) return []
    return fixtures.map((f, i) => {
      const fixture: FixtureEntry = {
        method: f.method,
        path_prefix: f.path_prefix,
        response: {
          status: f.response.status,
        },
      }

      // Add query if edited or original
      if (editingQuery[i] !== undefined) {
        const queryStr = editingQuery[i]
        if (queryStr.trim()) {
          try {
            fixture.query = JSON.parse(queryStr)
          } catch {
            // Ignore parse errors here; already validated
          }
        }
      } else if (f.query) {
        fixture.query = f.query
      }

      // Add response body
      if (editingBody[i] !== undefined) {
        const bodyStr = editingBody[i]
        if (bodyStr.trim()) {
          try {
            // Send the parsed value: the server field is json.RawMessage, so a
            // stringified body would be persisted as a quoted JSON string.
            fixture.response.body = JSON.parse(bodyStr)
          } catch {
            // Should not happen; already validated
          }
        }
      } else if (f.response.body !== undefined && f.response.body !== null) {
        // Explicit null/undefined check: a body of 0, false or "" is still a body,
        // and dropping it here would persist the fixture with no body at all.
        fixture.response.body = f.response.body
      } else if (f.response.body_text) {
        fixture.response.body_text = f.response.body_text
      }

      // Copy headers if present
      if (f.response.headers) {
        fixture.response.headers = f.response.headers
      }

      return fixture
    })
  }

  // Mutations
  const saveMutation = useMutation({
    mutationFn: async () => {
      if (!selectedVenue) throw new Error("No venue selected")
      const toSave = getFixturesToSave()
      await putFixtures(selectedVenue, toSave)
    },
    onSuccess: () => {
      setSaveMessages((prev) => ({
        ...prev,
        [selectedVenue || "unknown"]: {
          type: "success",
          text: t("pages.sandbox.save_success", "Fixtures saved"),
        },
      }))
      // Clear editing state
      setEditingBody({})
      setEditingQuery({})
      // Refetch fixtures
      queryClient.invalidateQueries({
        queryKey: ["sandbox", "fixtures", selectedVenue],
      })
      setTimeout(() => {
        setSaveMessages((prev) => {
          const updated = { ...prev }
          delete updated[selectedVenue || "unknown"]
          return updated
        })
      }, 3000)
    },
    onError: (error) => {
      setSaveMessages((prev) => ({
        ...prev,
        [selectedVenue || "unknown"]: {
          type: "error",
          text: (error as Error).message,
        },
      }))
    },
  })

  const reloadMutation = useMutation({
    mutationFn: reloadFixtures,
    onSuccess: () => {
      setResetMessage({
        type: "success",
        text: t("pages.sandbox.reload_success", "Fixtures reloaded"),
      })
      queryClient.invalidateQueries({ queryKey: ["sandbox", "fixtures"] })
      setTimeout(() => setResetMessage(null), 3000)
    },
    onError: (error) => {
      setResetMessage({
        type: "error",
        text: (error as Error).message,
      })
    },
  })

  const resetStateMutation = useMutation({
    mutationFn: resetSandboxState,
    onSuccess: () => {
      setResetMessage({
        type: "success",
        text: t("pages.sandbox.reset_success", "Simulator state reset"),
      })
      setTimeout(() => setResetMessage(null), 3000)
    },
    onError: (error) => {
      const err = error as Error
      if (err.message.includes("503")) {
        setResetMessage({
          type: "error",
          text: t(
            "pages.sandbox.reset_unavailable",
            "Simulator not available (503)",
          ),
        })
      } else {
        setResetMessage({
          type: "error",
          text: err.message,
        })
      }
    },
  })

  const canSave = !saveMutation.isPending && editedFixtureIndices.size > 0 && Object.keys(jsonErrors).length === 0

  return (
    <div className="flex h-full flex-col">
      <PageHeader title={t("navigation.sandbox", "Sandbox Mode")} />
      <div className="flex-1 overflow-auto p-3 lg:p-6">
        <div className="mx-auto w-full max-w-[1200px] space-y-6">
          {isLoading ? (
            <div className="text-muted-foreground py-6 text-sm">
              {t("labels.loading")}
            </div>
          ) : error ? (
            <div className="text-destructive py-6 text-sm">
              {t("pages.sandbox.load_error", "Failed to load sandbox status")}
            </div>
          ) : !status?.enabled ? (
            <div className="rounded-lg border border-yellow-200 bg-yellow-50 p-4">
              <p className="text-sm text-yellow-900">
                {t("pages.sandbox.disabled", "Sandbox mode is disabled")}
              </p>
            </div>
          ) : !status?.gateway_reachable ? (
            <div className="rounded-lg border border-red-200 bg-red-50 p-4">
              <p className="text-sm text-red-900">
                {t(
                  "pages.sandbox.gateway_unreachable",
                  "Gateway is not reachable",
                )}
              </p>
            </div>
          ) : (
            <div className="space-y-6">
              {/* Status Card */}
              <Card>
                <CardHeader>
                  <CardTitle>
                    {t("pages.sandbox.status", "Sandbox Status")}
                  </CardTitle>
                </CardHeader>
                <CardContent className="space-y-4">
                  <div className="flex items-center gap-2">
                    <div className="h-2 w-2 rounded-full bg-green-500" />
                    <span className="text-sm">
                      {t("pages.sandbox.enabled_label", "Enabled")}
                    </span>
                  </div>
                  {status?.fixtures_dir && (
                    <div className="space-y-2">
                      <div className="text-xs font-medium text-muted-foreground">
                        {t("pages.sandbox.fixtures_dir", "Fixtures Directory")}
                      </div>
                      <div className="rounded bg-muted px-2 py-1 font-mono text-xs text-muted-foreground break-all">
                        {status.fixtures_dir}
                      </div>
                    </div>
                  )}
                </CardContent>
              </Card>

              {/* Venues and Fixture Editor */}
              <div className="grid gap-6 md:grid-cols-4">
                {/* Venues List */}
                <div>
                  <Card>
                    <CardHeader>
                      <CardTitle className="text-base">
                        {t("pages.sandbox.venues", "Venues")}
                      </CardTitle>
                    </CardHeader>
                    <CardContent className="space-y-2">
                      {status?.venues && status.venues.length > 0 ? (
                        status.venues.map((venue) => (
                          <button
                            key={venue}
                            onClick={() => {
                              setSelectedVenue(venue)
                              setEditingBody({})
                              setEditingQuery({})
                              setSaveMessages({})
                            }}
                            className={`w-full rounded px-3 py-2 text-left text-sm transition ${
                              selectedVenue === venue
                                ? "bg-blue-100 text-blue-900 font-medium"
                                : "bg-muted hover:bg-muted/80"
                            }`}
                          >
                            {venue}
                          </button>
                        ))
                      ) : (
                        <p className="text-xs text-muted-foreground">
                          {t("pages.sandbox.no_venues", "No venues")}
                        </p>
                      )}
                    </CardContent>
                  </Card>
                </div>

                {/* Fixtures Editor */}
                <div className="md:col-span-3">
                  {!selectedVenue ? (
                    <Card>
                      <CardContent className="flex items-center justify-center py-8">
                        <p className="text-sm text-muted-foreground">
                          {t(
                            "pages.sandbox.select_venue",
                            "Select a venue to edit fixtures",
                          )}
                        </p>
                      </CardContent>
                    </Card>
                  ) : fixturesLoading ? (
                    <Card>
                      <CardContent className="flex items-center justify-center py-8">
                        <IconLoader className="h-5 w-5 animate-spin" />
                      </CardContent>
                    </Card>
                  ) : fixturesError ? (
                    <Card>
                      <CardContent className="flex items-center gap-2 py-4 text-sm text-red-600">
                        <IconX className="h-4 w-4" />
                        {(fixturesError as Error).message}
                      </CardContent>
                    </Card>
                  ) : !fixtures || fixtures.length === 0 ? (
                    <Card>
                      <CardContent className="flex items-center justify-center py-8">
                        <p className="text-sm text-muted-foreground">
                          {t(
                            "pages.sandbox.no_fixtures",
                            "No fixtures for this venue",
                          )}
                        </p>
                      </CardContent>
                    </Card>
                  ) : (
                    <Card>
                      <CardHeader>
                        <CardTitle className="text-base">
                          {t("pages.sandbox.fixtures", "Fixtures")} ({fixtures.length})
                        </CardTitle>
                        <CardDescription>
                          {t(
                            "pages.sandbox.fixtures_desc",
                            "Edit the response body as JSON. Invalid JSON will be rejected.",
                          )}
                        </CardDescription>
                      </CardHeader>
                      <CardContent className="space-y-6">
                        {/* Save message */}
                        {saveMessages[selectedVenue] && (
                          <div
                            className={`flex items-center gap-2 rounded px-3 py-2 text-sm ${
                              saveMessages[selectedVenue].type === "success"
                                ? "bg-green-100 text-green-800"
                                : "bg-red-100 text-red-800"
                            }`}
                          >
                            {saveMessages[selectedVenue].type === "success" ? (
                              <IconCheck className="h-4 w-4" />
                            ) : (
                              <IconX className="h-4 w-4" />
                            )}
                            {saveMessages[selectedVenue].text}
                          </div>
                        )}

                        {/* Fixtures list */}
                        <div className="space-y-4 max-h-96 overflow-y-auto">
                          {fixtures.map((fixture, i) => (
                            <div
                              key={i}
                              className="rounded border border-border/40 bg-muted/30 p-3 space-y-3"
                            >
                              {/* Header */}
                              <div className="flex items-center justify-between">
                                <div className="font-mono text-xs">
                                  <span className="font-bold text-blue-600">
                                    {fixture.method}
                                  </span>{" "}
                                  {fixture.path_prefix}
                                </div>
                                <span className="text-xs font-medium px-2 py-1 rounded bg-gray-200">
                                  {fixture.response.status}
                                </span>
                              </div>

                              {/* Query params if present */}
                              {fixture.query || editingQuery[i] !== undefined ? (
                                <div className="space-y-1">
                                  <label className="text-xs font-medium">
                                    {t("pages.sandbox.query", "Query Params")}
                                  </label>
                                  <Textarea
                                    placeholder='{"key": "value"}'
                                    value={
                                      editingQuery[i] !== undefined
                                        ? editingQuery[i]
                                        : fixture.query
                                          ? JSON.stringify(fixture.query)
                                          : "{}"
                                    }
                                    onChange={(e) =>
                                      setEditingQuery((prev) => ({
                                        ...prev,
                                        [i]: e.target.value,
                                      }))
                                    }
                                    className="font-mono text-xs"
                                    rows={2}
                                  />
                                </div>
                              ) : null}

                              {/* Response body */}
                              <div className="space-y-1">
                                <label className="text-xs font-medium">
                                  {t("pages.sandbox.response_body", "Response Body")}
                                </label>
                                <Textarea
                                  placeholder='{"result": "..."}'
                                  value={
                                    editingBody[i] !== undefined
                                      ? editingBody[i]
                                      : formatBody(fixture.response)
                                  }
                                  onChange={(e) => handleBodyChange(i, e.target.value)}
                                  className={`font-mono text-xs ${
                                    jsonErrors[i] ? "border-red-500" : ""
                                  }`}
                                  rows={5}
                                />
                                {jsonErrors[i] && (
                                  <p className="text-xs text-red-600">
                                    {jsonErrors[i]}
                                  </p>
                                )}
                              </div>
                            </div>
                          ))}
                        </div>

                        {/* Save button */}
                        <Button
                          onClick={() => saveMutation.mutate()}
                          disabled={!canSave}
                          className="w-full"
                        >
                          {saveMutation.isPending ? (
                            <>
                              <IconLoader className="mr-2 h-4 w-4 animate-spin" />
                              {t("common.saving", "Saving...")}
                            </>
                          ) : (
                            t("common.save", "Save")
                          )}
                        </Button>
                      </CardContent>
                    </Card>
                  )}
                </div>
              </div>

              {/* Action Buttons */}
              <Card>
                <CardHeader>
                  <CardTitle className="text-base">
                    {t("pages.sandbox.actions", "Actions")}
                  </CardTitle>
                </CardHeader>
                <CardContent className="space-y-4">
                  {resetMessage && (
                    <div
                      className={`flex items-center gap-2 rounded px-3 py-2 text-sm ${
                        resetMessage.type === "success"
                          ? "bg-green-100 text-green-800"
                          : "bg-red-100 text-red-800"
                      }`}
                    >
                      {resetMessage.type === "success" ? (
                        <IconCheck className="h-4 w-4" />
                      ) : (
                        <IconX className="h-4 w-4" />
                      )}
                      {resetMessage.text}
                    </div>
                  )}
                  <div className="grid gap-2 grid-cols-2">
                    <Button
                      variant="outline"
                      onClick={() => reloadMutation.mutate()}
                      disabled={reloadMutation.isPending}
                    >
                      {reloadMutation.isPending ? (
                        <IconLoader className="mr-2 h-4 w-4 animate-spin" />
                      ) : null}
                      {t("pages.sandbox.reload", "Reload Fixtures")}
                    </Button>
                    <Button
                      variant="outline"
                      onClick={() => resetStateMutation.mutate()}
                      disabled={resetStateMutation.isPending}
                    >
                      {resetStateMutation.isPending ? (
                        <IconLoader className="mr-2 h-4 w-4 animate-spin" />
                      ) : null}
                      {t("pages.sandbox.reset_state", "Reset State")}
                    </Button>
                  </div>
                </CardContent>
              </Card>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

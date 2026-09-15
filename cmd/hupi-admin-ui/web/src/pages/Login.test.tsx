import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Login } from "./Login";
import { AuthProvider } from "../lib/auth";

function mockResponse(status: number, body: unknown): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    text: async () => JSON.stringify(body),
  } as Response;
}

function renderLogin() {
  return render(
    <MemoryRouter initialEntries={["/login"]}>
      <AuthProvider>
        <Login />
      </AuthProvider>
    </MemoryRouter>,
  );
}

describe("Login", () => {
  beforeEach(() => {
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  it("submits a Basic Auth credential built from name + token and stores it on success", async () => {
    const fetchMock = vi.spyOn(global, "fetch").mockResolvedValue(mockResponse(200, { name: "alice" }));

    renderLogin();

    fireEvent.change(screen.getByPlaceholderText("alice"), { target: { value: "alice" } });
    fireEvent.change(screen.getByPlaceholderText("raw operator token"), { target: { value: "secret-token" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/whoami");
    const headers = init.headers as Record<string, string>;
    expect(headers.Authorization).toBe(`Basic ${btoa("alice:secret-token")}`);

    await waitFor(() => {
      const stored = JSON.parse(sessionStorage.getItem("hupi_admin_ui_auth") ?? "{}");
      expect(stored.name).toBe("alice");
      expect(stored.basicAuthValue).toBe(btoa("alice:secret-token"));
    });
  });

  it("shows an inline error and stores nothing on a 401", async () => {
    vi.spyOn(global, "fetch").mockResolvedValue(mockResponse(401, { error: "unauthorized" }));

    renderLogin();

    fireEvent.change(screen.getByPlaceholderText("raw operator token"), { target: { value: "wrong-token" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByText(/invalid operator name or token/i)).toBeInTheDocument();
    expect(sessionStorage.getItem("hupi_admin_ui_auth")).toBeNull();
  });
});

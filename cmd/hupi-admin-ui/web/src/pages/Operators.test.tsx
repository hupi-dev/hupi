import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Operators } from "./Operators";
import { ToastProvider } from "../components/Toast";

function mockResponse(status: number, body: unknown): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    text: async () => JSON.stringify(body),
  } as Response;
}

function renderOperators() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <Operators />
      </ToastProvider>
    </MemoryRouter>,
  );
}

describe("Operators screen", () => {
  beforeEach(() => {
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  it("lists existing operators from GET /api/operators", async () => {
    vi.spyOn(global, "fetch").mockResolvedValue(
      mockResponse(200, [{ Name: "alice", CreatedAt: "2026-01-01T00:00:00Z", RevokedAt: null }]),
    );

    renderOperators();

    expect(await screen.findByText("alice")).toBeInTheDocument();
    expect(screen.getByText("Active")).toBeInTheDocument();
  });

  it("shows an empty state when there are no operators yet", async () => {
    vi.spyOn(global, "fetch").mockResolvedValue(mockResponse(200, null));

    renderOperators();

    expect(await screen.findByText(/no operators provisioned yet/i)).toBeInTheDocument();
  });

  it("creates a new operator and shows the raw token exactly once", async () => {
    const fetchMock = vi
      .spyOn(global, "fetch")
      .mockResolvedValueOnce(mockResponse(200, [])) // initial GET /operators
      .mockResolvedValueOnce(mockResponse(201, { raw_token: "hupi_op_abc123" })) // POST /operators
      .mockResolvedValueOnce(
        mockResponse(200, [{ Name: "bob", CreatedAt: "2026-01-01T00:00:00Z", RevokedAt: null }]),
      ); // reload GET /operators

    renderOperators();
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    fireEvent.click(screen.getByRole("button", { name: /new operator/i }));
    fireEvent.change(screen.getByPlaceholderText("alice"), { target: { value: "bob" } });
    fireEvent.click(screen.getByRole("button", { name: /^create$/i }));

    expect(await screen.findByText("hupi_op_abc123")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/api/operators",
      expect.objectContaining({ method: "POST", body: JSON.stringify({ name: "bob" }) }),
    );
  });
});

export interface CurrentUser {
  id: string;
  name: string;
  username: string;
  email: string;
  avatar: string;
  role: string;
}

// JWTs are held only in a server-set HttpOnly cookie. Remove legacy storage
// during upgrade; this module never reads or writes authentication cookies.
export const auth = {
  clearLegacyToken(): void {
    if (typeof window !== "undefined") localStorage.removeItem("artex_token");
  },
  async logout(): Promise<void> {
    this.clearLegacyToken();
    if (process.env.NEXT_PUBLIC_MOCK === "1") return;
    const response = await fetch("/api/auth/logout", {
      method: "POST",
      credentials: "include",
      headers: { "X-Artex-CSRF": "1" },
    });
    if (!response.ok) throw new Error("注销失败，请重试");
  },
  getCurrentUser(): CurrentUser {
    return { id: "1", name: "ARTEX", username: "ARTEX", email: "", avatar: "", role: "operator" };
  },
};

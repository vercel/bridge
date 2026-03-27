import type { Metadata } from "next";
import { formatTitle } from "../utils/format";

export const metadata: Metadata = {
  title: "Todo Service",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body style={{ fontFamily: "system-ui, sans-serif", margin: 0, padding: "2rem", maxWidth: 600, marginInline: "auto" }}>
        {children}
      </body>
    </html>
  );
}

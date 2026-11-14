import QRCode from "qrcode";

// QR rendering with phone-friendly margins (M0-TASKS.md step 8:
// "big, quiet zone — phones").

export async function renderQr(canvas: HTMLCanvasElement, text: string): Promise<void> {
  await QRCode.toCanvas(canvas, text, {
    width: 320,
    margin: 4, // generous quiet zone for phone cameras
    errorCorrectionLevel: "M",
    color: { dark: "#111", light: "#fff" },
  });
}
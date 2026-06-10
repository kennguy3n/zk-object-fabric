import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import { App } from "./App";
import { ToastProvider } from "./ui/toast";
import { TooltipProvider } from "./ui/tooltip";
import "./index.css";

const container = document.getElementById("root");
if (!container) {
  throw new Error("#root not found in index.html");
}

createRoot(container).render(
  <StrictMode>
    <BrowserRouter>
      <TooltipProvider delayDuration={200}>
        <ToastProvider>
          <App />
        </ToastProvider>
      </TooltipProvider>
    </BrowserRouter>
  </StrictMode>,
);

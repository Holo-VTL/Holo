import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import "./i18n";
import "./styles/global.css";
import { ThemeProvider } from "./app/ThemeContext";
import { ToastProvider } from "./components/Toast";
import { App } from "./App";

const basePath = import.meta.env.BASE_URL.replace(/\/$/, "");
const routerBase = basePath && basePath !== "/" ? basePath : undefined;

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <ThemeProvider>
      <ToastProvider>
        <BrowserRouter basename={routerBase}>
          <App />
        </BrowserRouter>
      </ToastProvider>
    </ThemeProvider>
  </React.StrictMode>
);

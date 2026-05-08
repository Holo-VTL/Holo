import { Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "./components/AppShell";
import { DashboardPage } from "./pages/DashboardPage";
import { StoragePage } from "./pages/StoragePage";
import { ResourcesPage } from "./pages/ResourcesPage";
import { ResourceManagePage } from "./pages/ResourceManagePage";
import { TargetsPage } from "./pages/TargetsPage";
import { AboutPage } from "./pages/AboutPage";

export function App() {
  return (
    <Routes>
      <Route path="/" element={<AppShell />}>
        <Route index element={<DashboardPage />} />
        <Route path="storage" element={<StoragePage />} />
        <Route path="resources" element={<ResourcesPage />} />
        <Route path="resources/:libraryId/manage" element={<ResourceManagePage />} />
        <Route path="targets" element={<TargetsPage />} />
        <Route path="about" element={<AboutPage />} />
        <Route path="audit" element={<Navigate to="/" replace />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

import { Theme } from "@radix-ui/themes";
import "@radix-ui/themes/styles.css";
import React from "react";
import ReactDOM from "react-dom/client";
import { Route, RouterProvider, createHashRouter, createRoutesFromElements } from "react-router-dom";

import Layout from "@/layout/layout.tsx";

import Dashboard from "@/pages/dashboard/index.tsx";

import App from "./App.tsx";
import "./index.css";

const router = createHashRouter(
  createRoutesFromElements(
    <Route path="/" element={<App />}>
      <Route path="/" element={<Layout />}>
        <Route index element={<Dashboard />} />
      </Route>
    </Route>,
  ),
);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <Theme accentColor="mint" panelBackground="solid" scaling="100%" radius="large">
      <RouterProvider router={router} />
    </Theme>
  </React.StrictMode>,
);

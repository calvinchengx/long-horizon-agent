import { render } from "preact";
import { LocationProvider, Route, Router } from "preact-iso";
import { Mission } from "./Mission";
import { Missions } from "./Missions";
import "./style.css";

function NotFound() {
  return <p class="empty">Nothing here. <a href="/">All missions</a></p>;
}

function App() {
  return (
    <LocationProvider>
      <header class="top">
        <a href="/" class="brand">LHA <span>missions</span></a>
      </header>
      <main>
        <Router>
          <Route path="/" component={Missions} />
          <Route path="/missions/:id" component={({ id }: { id: string }) => <Mission id={decodeURIComponent(id)} />} />
          <Route default component={NotFound} />
        </Router>
      </main>
    </LocationProvider>
  );
}

render(<App />, document.getElementById("app")!);

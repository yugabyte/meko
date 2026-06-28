// Home centre-body: the "Welcome to AMP" hero. Header (search, theme
// toggle) and the signed-in user / sign-out now live in the app shell
// (AppLayout), so this page only owns its content.

import { useAuth } from "@/auth/AuthContext";

export default function WelcomePage() {
  const { user } = useAuth();

  return (
    <div className="flex min-h-full flex-col items-center justify-center px-4 py-16 text-center">
      <h1 className="bg-gradient-to-br from-primary to-muted-foreground bg-clip-text text-5xl font-extrabold tracking-tight text-transparent sm:text-7xl">
        Welcome to AMP
      </h1>
      {user && (
        <p className="mt-6 text-lg text-muted-foreground">
          You&apos;re signed in as{" "}
          <span className="font-medium text-foreground">{user.name}</span>
          {" "}({user.email}).
        </p>
      )}
    </div>
  );
}

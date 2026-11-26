import { Code, ConnectError } from "@connectrpc/connect";

/** What went wrong, for a toast or an inline error. The server's own text is shown where it says what to change. */
export function errorText(error: unknown): string {
  const e = ConnectError.from(error);
  switch (e.code) {
    case Code.InvalidArgument:
    case Code.FailedPrecondition:
      return e.rawMessage || "This cannot be saved as it is.";
    case Code.PermissionDenied:
      return e.rawMessage || "You are not allowed to do that.";
    case Code.NotFound:
      return "It no longer exists.";
    case Code.AlreadyExists:
      // Also a refusal that says what to do instead: settle not yet, send
      // again too late, or edit the payload to fit the action's schema.
      return e.rawMessage || "Someone already changed this. Reload the page and try again.";
    case Code.Aborted:
      return "Someone already changed this. Reload the page and try again.";
    case Code.Unauthenticated:
      return "You are signed out. Sign in again.";
    case Code.Unavailable:
    case Code.DeadlineExceeded:
    case Code.Canceled:
      return "Could not reach pgmandate. Check the connection.";
    default:
      if (error instanceof TypeError) return "Could not reach pgmandate. Check the connection.";
      return "Something went wrong in pgmandate.";
  }
}

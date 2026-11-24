// Adds the provider's sign-up parameters (OIDC_REGISTER_PARAMS, by default the standard prompt=create) to the
// authorization redirect when the BFF asked for sign-up: /auth/login?registration=...&signup=1.
def signUp = request.uri.path == '/auth/login' && request.queryParams.getFirst('signup') == '1'
return next.handle(context, request).thenOnResult { response ->
    def location = response.headers.getFirst('Location')
    if (signUp && response.status.redirection && location != null) {
        response.headers.put('Location', location + (location.contains('?') ? '&' : '?') + signUpParams)
    }
}
